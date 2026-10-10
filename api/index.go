package handler

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"ai-scraper/scraper"
)

type ScrapeResult struct {
	ID          string
	URL         string
	Title       string
	Prompt      string
	Mode        string
	Duration    time.Duration
	CharCount   int
	JSONRaw     string
	Data        []map[string]interface{}
	Columns     []string
	Error       string
	Timestamp   time.Time
	Images      []scraper.ImageInfo
	Colors      []string
	ColorCounts map[string]int
	Fonts       []string
	Stylesheets []string
	Structure   string
	Framework   string
	Meta        scraper.MetaInfo
}

var (
	tmpl     *template.Template
	tmplOnce sync.Once
	store    = map[string]*ScrapeResult{}
	storeMu  sync.RWMutex
	idCounter int
	router   http.Handler
	routerOnce sync.Once
)

func getTmpl() *template.Template {
	tmplOnce.Do(func() {
		var err error
		// Vercel wd is project root; try both locations
		for _, p := range []string{"templates/index.html", "../templates/index.html", "./templates/index.html"} {
			tmpl, err = template.ParseFiles(p)
			if err == nil {
				return
			}
		}
		log.Printf("template parse failed: %v", err)
	})
	return tmpl
}

func getRouter() http.Handler {
	routerOnce.Do(func() {
		r := chi.NewRouter()
		r.Use(middleware.Recoverer)
		r.Use(middleware.Compress(5))
		// static files
		for _, dir := range []string{"static", "../static", "./static"} {
			if _, err := os.Stat(dir); err == nil {
				r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir(dir))))
				break
			}
		}
		r.Get("/", handleIndex)
		r.Post("/scrape", handleScrape)
		r.Get("/export/csv/{id}", handleCSV)
		r.Get("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
		router = r
	})
	return router
}

// Handler is the Vercel serverless entry point.
func Handler(w http.ResponseWriter, r *http.Request) {
	getRouter().ServeHTTP(w, r)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	t := getTmpl()
	if t == nil {
		http.Error(w, "template not found", 500)
		return
	}
	hasKey := os.Getenv("GROQ_API_KEY") != ""
	t.Execute(w, map[string]interface{}{
		"HasKey": hasKey,
		"Model":  envOr("GROQ_MODEL", "qwen/qwen3-32b"),
	})
}

func handleScrape(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		renderError(w, "Invalid form data")
		return
	}
	targetURL := strings.TrimSpace(r.FormValue("url"))
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	mode := r.FormValue("mode")
	if mode == "" {
		mode = "auto"
	}
	if targetURL == "" {
		renderError(w, "Please enter a URL to scrape.")
		return
	}
	if !strings.HasPrefix(targetURL, "http") {
		targetURL = "https://" + targetURL
	}
	if prompt == "" {
		prompt = "Extract all main items on the page with their name/title, price if present, and any key details."
	}

	var fetch *scraper.FetchResult
	var fetchErr error
	var actualMode string

	if mode == "js" {
		actualMode = "js (headless Chrome)"
		fetch, fetchErr = scraper.FetchWithJS(targetURL)
		// On Vercel there is no Chrome — fall back to static with a note
		if fetchErr != nil {
			actualMode = "static (JS unavailable on serverless)"
			fetch, fetchErr = scraper.FetchStatic(targetURL)
		}
	} else if mode == "static" {
		actualMode = "static"
		fetch, fetchErr = scraper.FetchStatic(targetURL)
	} else {
		actualMode = "static"
		fetch, fetchErr = scraper.FetchStatic(targetURL)
		if fetchErr == nil && len(strings.Fields(fetch.Text)) < 60 {
			actualMode = "js (auto fallback)"
			if jsFetch, err := scraper.FetchWithJS(targetURL); err == nil && len(jsFetch.Text) > len(fetch.Text) {
				fetch = jsFetch
			} else {
				actualMode = "static"
			}
		}
	}

	if fetchErr != nil {
		renderError(w, fmt.Sprintf("Fetch failed: %v", fetchErr))
		return
	}
	if strings.TrimSpace(fetch.Text) == "" {
		renderError(w, "Page returned no extractable text. Try a different URL.")
		return
	}

	parsed, prettyJSON, aiErr := scraper.ExtractWithGroqFromFetch(fetch, prompt)
	if aiErr != nil {
		renderError(w, fmt.Sprintf("AI extraction failed: %v", aiErr))
		return
	}

	var rows []map[string]interface{}
	var cols []string
	colSet := map[string]bool{}
	switch v := parsed.(type) {
	case []interface{}:
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				rows = append(rows, m)
				for k := range m {
					if !colSet[k] {
						cols = append(cols, k)
						colSet[k] = true
					}
				}
			}
		}
	case map[string]interface{}:
		rows = append(rows, v)
		for k := range v {
			cols = append(cols, k)
		}
	default:
		rows = []map[string]interface{}{{"value": fmt.Sprint(parsed)}}
		cols = []string{"value"}
	}

	storeMu.Lock()
	idCounter++
	id := fmt.Sprintf("%d", idCounter)
	res := &ScrapeResult{
		ID: id, URL: fetch.URL, Title: fetch.Title, Prompt: prompt,
		Mode: actualMode, Duration: fetch.Duration, CharCount: fetch.CharCount,
		JSONRaw: prettyJSON, Data: rows, Columns: cols, Timestamp: time.Now(),
		Images: fetch.Images, Colors: fetch.Colors, ColorCounts: fetch.ColorCounts, Fonts: fetch.Fonts,
		Stylesheets: fetch.Stylesheets, Structure: fetch.Structure, Framework: fetch.Framework, Meta: fetch.Meta,
	}
	store[id] = res
	storeMu.Unlock()

	renderResults(w, res)
}

func renderResults(w http.ResponseWriter, res *ScrapeResult) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	count := len(res.Data)
	hasRows := count > 0
	var tableHTML string
	if hasRows {
		var buf bytes.Buffer
		buf.WriteString(`<div class="table-wrap"><table><thead><tr>`)
		buf.WriteString(`<th style="width:44px">#</th>`)
		for _, c := range res.Columns {
			buf.WriteString(`<th>` + template.HTMLEscapeString(humanize(c)) + `</th>`)
		}
		buf.WriteString(`</tr></thead><tbody>`)
		for i, row := range res.Data {
			buf.WriteString(`<tr>`)
			buf.WriteString(fmt.Sprintf(`<td class="muted">%d</td>`, i+1))
			for _, col := range res.Columns {
				val := fmt.Sprint(row[col])
				if val == "<nil>" || val == "" {
					val = "-"
				}
				display := val
				if len(display) > 180 {
					display = display[:180] + "…"
				}
				buf.WriteString(`<td title="` + template.HTMLEscapeString(val) + `">` + template.HTMLEscapeString(display) + `</td>`)
			}
			buf.WriteString(`</tr>`)
		}
		buf.WriteString(`</tbody></table></div>`)
		tableHTML = buf.String()
	} else {
		tableHTML = `<div class="empty-state"><div class="empty-icon">` + iconSearch() + `</div><p>No items matched your prompt.</p><span>Try a broader instruction like "extract all products with name and price"</span></div>`
	}
	var paletteHTML string
	if len(res.Colors) > 0 {
		var pb bytes.Buffer
		pb.WriteString(`<div class="palette-grid">`)
		for _, c := range res.Colors {
			esc := template.HTMLEscapeString(c)
			count := 0
			if res.ColorCounts != nil {
				count = res.ColorCounts[strings.ToLower(c)]
				if count == 0 {
					count = res.ColorCounts[c]
				}
			}
			badge := ""
			if count > 1 {
				badge = fmt.Sprintf(`<span class="swatch-count">×%d</span>`, count)
			}
			pb.WriteString(fmt.Sprintf(`<button class="swatch" onclick="copyColor('%s')" title="Copy %s"><span class="swatch-color" style="background:%s"></span><span class="swatch-hex">%s</span>%s</button>`, esc, esc, esc, esc, badge))
		}
		pb.WriteString(`</div>`)
		if len(res.Colors) > 12 {
			pb.WriteString(fmt.Sprintf(`<div class="palette-foot">%d colors extracted · sorted by frequency · click to copy</div>`, len(res.Colors)))
		} else {
			pb.WriteString(`<div class="palette-foot">Click any swatch to copy its value</div>`)
		}
		paletteHTML = pb.String()
	} else {
		paletteHTML = `<div class="empty-state small"><p>No palette detected</p><span>Page may use images for color or inline styles only</span></div>`
	}
	var fontsHTML string
	if len(res.Fonts) > 0 {
		var fb bytes.Buffer
		fb.WriteString(`<div class="font-list">`)
		for _, f := range res.Fonts {
			fb.WriteString(fmt.Sprintf(`<span class="font-pill" style="font-family:'%s',system-ui,sans-serif">%s</span>`, template.HTMLEscapeString(f), template.HTMLEscapeString(f)))
		}
		fb.WriteString(`</div>`)
		fontsHTML = fb.String()
	} else {
		fontsHTML = `<span class="muted" style="font-size:12px">No custom fonts detected (system fonts only)</span>`
	}
	var imagesHTML string
	if len(res.Images) > 0 {
		var ib bytes.Buffer
		ib.WriteString(`<div class="image-grid">`)
		for i, im := range res.Images {
			srcEsc := template.HTMLEscapeString(im.Src)
			altEsc := template.HTMLEscapeString(firstNonEmpty(im.Alt, fmt.Sprintf("image %d", i+1)))
			kindEsc := template.HTMLEscapeString(im.Kind)
			ib.WriteString(fmt.Sprintf(`<div class="img-card">
				<div class="img-thumb"><img src="%s" alt="%s" loading="lazy" onerror="this.parentElement.innerHTML='<span class=img-fallback>·</span>'"/></div>
				<div class="img-meta"><span class="img-alt" title="%s">%s</span><span class="img-kind">%s</span></div>
				<div class="img-actions"><a href="%s" target="_blank" rel="noopener" class="btn btn-ghost btn-xs">Open</a><button class="btn btn-ghost btn-xs" onclick="copyText('%s','Image URL copied')">Copy URL</button></div>
			</div>`, srcEsc, altEsc, altEsc, altEsc, kindEsc, srcEsc, srcEsc))
			if i >= 47 {
				ib.WriteString(fmt.Sprintf(`<div class="img-more">+%d more images truncated</div>`, len(res.Images)-48))
				break
			}
		}
		ib.WriteString(`</div>`)
		imagesHTML = ib.String()
	} else {
		imagesHTML = `<div class="empty-state small"><p>No images found</p><span>Page may use CSS backgrounds or lazy-load outside viewport</span></div>`
	}
	structEsc := template.HTMLEscapeString(res.Structure)
	if structEsc == "" {
		structEsc = "No heading structure detected."
	}
	structHTML := fmt.Sprintf(`<pre class="structure-pre">%s</pre>`, structEsc)
	if res.Framework != "" {
		structHTML = fmt.Sprintf(`<div class="framework-badge">%s</div>`, template.HTMLEscapeString(res.Framework)) + structHTML
	}
	if len(res.Stylesheets) > 0 {
		var slb bytes.Buffer
		slb.WriteString(`<div class="sheet-list">`)
		for _, s := range res.Stylesheets {
			slb.WriteString(fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener" class="sheet-link">%s</a>`, template.HTMLEscapeString(s), template.HTMLEscapeString(s)))
		}
		slb.WriteString(`</div>`)
		structHTML += slb.String()
	}
	jsonEscaped := template.HTMLEscapeString(res.JSONRaw)
	designJSON := ""
	if len(res.Images) > 0 || len(res.Colors) > 0 {
		type designExport struct {
			URL         string              `json:"url"`
			Title       string              `json:"title"`
			Colors      []string            `json:"colors"`
			ColorCounts map[string]int      `json:"color_counts,omitempty"`
			Fonts       []string            `json:"fonts"`
			Images      []scraper.ImageInfo `json:"images"`
			Framework   string              `json:"framework,omitempty"`
			Structure   string              `json:"structure"`
		}
		d := designExport{URL: res.URL, Title: res.Title, Colors: res.Colors, ColorCounts: res.ColorCounts, Fonts: res.Fonts, Images: res.Images, Framework: res.Framework, Structure: res.Structure}
		if b, err := json.MarshalIndent(d, "", "  "); err == nil {
			designJSON = string(b)
		}
	}
	designJSONEsc := template.HTMLEscapeString(designJSON)

	fmt.Fprintf(w, `
<div id="results" class="results-card">
  <div class="results-head">
    <div class="results-meta">
      <span class="badge badge-ok">%d items</span>
      <span class="meta-dot">·</span>
      <span class="meta-text">%s</span>
      <span class="meta-dot">·</span>
      <span class="meta-text">%s</span>
      <span class="meta-dot">·</span>
      <span class="meta-text">%d chars</span>
      <span class="meta-dot">·</span>
      <span class="meta-text">%d images</span>
      <span class="meta-dot">·</span>
      <span class="meta-text">%d colors</span>
    </div>
    <div class="results-actions">
      <button class="btn btn-ghost" onclick="copyJSON('%s')">`+iconCopy()+` Copy JSON</button>
      <a class="btn btn-ghost" href="/export/csv/%s" download>`+iconDownload()+` Export CSV</a>
    </div>
  </div>

  <div class="title-row">
    <span class="title-label">Page</span>
    <span class="title-value">%s</span>
  </div>
  <div class="url-row"><a href="%s" target="_blank" rel="noopener">%s</a></div>

  <div class="tabs" id="resultTabs">
    <button class="tab active" data-tab="table" onclick="switchTab('table')">`+iconTable()+` Table</button>
    <button class="tab" data-tab="json" onclick="switchTab('json')">`+iconCode()+` JSON</button>
    <button class="tab" data-tab="design" onclick="switchTab('design')">`+iconPalette()+` Design</button>
    <button class="tab" data-tab="images" onclick="switchTab('images')">`+iconImage()+` Images <span class="tab-count">%d</span></button>
    <button class="tab" data-tab="structure" onclick="switchTab('structure')">`+iconStructure()+` Structure</button>
  </div>

  <div class="tab-panel active" id="panel-table">
    %s
  </div>
  <div class="tab-panel" id="panel-json">
    <div class="json-wrap">
      <button class="btn btn-ghost json-copy" onclick="copyJSON('%s')">`+iconCopy()+` Copy</button>
      <pre id="jsonPre-%s">%s</pre>
    </div>
    <textarea id="jsonData-%s" style="display:none">%s</textarea>
  </div>
  <div class="tab-panel" id="panel-design">
    <div class="design-grid">
      <div class="design-section">
        <h4 class="design-title">Color palette <span class="design-count">%d</span></h4>
        %s
      </div>
      <div class="design-section">
        <h4 class="design-title">Typography</h4>
        %s
      </div>
    </div>
    %s
    %s
  </div>
  <div class="tab-panel" id="panel-images">
    <div class="panel-head-row"><h4 class="design-title">Images <span class="design-count">%d</span></h4><button class="btn btn-ghost btn-xs" onclick="copyText('%s','All image URLs copied')">Copy all URLs</button></div>
    %s
  </div>
  <div class="tab-panel" id="panel-structure">
    <h4 class="design-title">Site structure</h4>
    %s
  </div>
</div>
<script>
  if(window.__scraperInit) window.__scraperInit();
</script>
`,
		count,
		template.HTMLEscapeString(res.Mode),
		fmt.Sprintf("%.1fs", res.Duration.Seconds()),
		res.CharCount,
		len(res.Images),
		len(res.Colors),
		res.ID,
		res.ID,
		template.HTMLEscapeString(firstNonEmpty(res.Title, res.URL)),
		template.HTMLEscapeString(res.URL),
		template.HTMLEscapeString(res.URL),
		len(res.Images),
		tableHTML,
		res.ID,
		res.ID,
		jsonEscaped,
		res.ID,
		jsonEscaped,
		len(res.Colors),
		paletteHTML,
		fontsHTML,
		func() string {
			if designJSONEsc != "" {
				return fmt.Sprintf(`<div class="design-section" style="margin-top:14px"><h4 class="design-title">Design JSON</h4><div class="json-wrap"><button class="btn btn-ghost json-copy" onclick="copyText(document.getElementById('designJson-%s').value,'Design JSON copied')">`+iconCopy()+` Copy</button><pre>%s</pre></div><textarea id="designJson-%s" style="display:none">%s</textarea></div>`, res.ID, designJSONEsc, res.ID, designJSONEsc)
			}
			return ""
		}(),
		func() string {
			if res.Meta.Description != "" || res.Meta.OGImage != "" {
				og := ""
				if res.Meta.OGImage != "" {
					og = fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener" class="sheet-link">OG: %s</a>`, template.HTMLEscapeString(res.Meta.OGImage), template.HTMLEscapeString(res.Meta.OGImage))
				}
				return fmt.Sprintf(`<div class="design-section" style="margin-top:14px"><h4 class="design-title">Meta</h4><p class="meta-desc">%s</p>%s</div>`, template.HTMLEscapeString(res.Meta.Description), og)
			}
			return ""
		}(),
		len(res.Images),
		template.HTMLEscapeString(strings.Join(func() []string { var a []string; for _, im := range res.Images { a = append(a, im.Src) }; return a }(), "\n")),
		imagesHTML,
		structHTML,
	)
}

func renderError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnprocessableEntity)
	fmt.Fprintf(w, `<div id="results" class="results-card error-card"><div class="error-icon">`+iconAlert()+`</div><p class="error-msg">%s</p><span class="error-hint">Check the URL and prompt, then try again.</span></div>`, template.HTMLEscapeString(msg))
}

func handleCSV(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	storeMu.RLock()
	res, ok := store[id]
	storeMu.RUnlock()
	if !ok {
		http.Error(w, "Result not found or expired", 404)
		return
	}
	if len(res.Data) == 0 {
		http.Error(w, "No data to export", 400)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=scrape-%s.csv", id))
	cw := csv.NewWriter(w)
	_ = cw.Write(res.Columns)
	for _, row := range res.Data {
		record := make([]string, len(res.Columns))
		for i, col := range res.Columns {
			record[i] = fmt.Sprint(row[col])
		}
		_ = cw.Write(record)
	}
	cw.Flush()
}

func humanize(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	parts := strings.Fields(s)
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func iconCopy() string     { return `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v3"/></svg>` }
func iconDownload() string { return `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>` }
func iconTable() string    { return `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="3" width="18" height="18" rx="2"/><line x1="3" y1="9" x2="21" y2="9"/><line x1="3" y1="15" x2="21" y2="15"/><line x1="9" y1="3" x2="9" y2="21"/><line x1="15" y1="3" x2="15" y2="21"/></svg>` }
func iconCode() string     { return `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="16 18 22 12 16 6"/><polyline points="8 6 2 12 8 18"/></svg>` }
func iconAlert() string    { return `<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg>` }
func iconSearch() string   { return `<svg width="28" height="28" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/></svg>` }
func iconPalette() string  { return `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="13.5" cy="6.5" r="0.5" fill="currentColor"/><circle cx="17.5" cy="10.5" r="0.5" fill="currentColor"/><circle cx="8.5" cy="7.5" r="0.5" fill="currentColor"/><circle cx="6.5" cy="12.5" r="0.5" fill="currentColor"/><path d="M12 2C6.5 2 2 6.5 2 12s4.5 10 10 10c.926 0 1.648-.746 1.648-1.688 0-.437-.18-.835-.437-1.125-.29-.289-.438-.652-.438-1.125a1.64 1.64 0 0 1 1.668-1.668h1.996c3.051 0 5.555-2.503 5.555-5.554C21.965 6.012 17.461 2 12 2z"/></svg>` }
func iconImage() string    { return `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="9" cy="9" r="2"/><path d="M21 15l-5-5L5 21"/></svg>` }
func iconStructure() string { return `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/></svg>` }

var _ = json.Marshal
