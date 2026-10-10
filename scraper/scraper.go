package scraper

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/chromedp"
)

// FetchResult holds raw fetch data plus design-intelligence signals.
type FetchResult struct {
	HTML        string
	Text        string
	Title       string
	URL         string
	Duration    time.Duration
	CharCount   int
	Images      []ImageInfo `json:"images"`
	Colors      []string    `json:"colors"`
	ColorCounts map[string]int `json:"color_counts,omitempty"`
	Fonts       []string    `json:"fonts"`
	Stylesheets []string    `json:"stylesheets"`
	Structure   string      `json:"structure"`
	Audio       []AudioInfo `json:"audio"`
	Framework   string      `json:"framework"`
	TechStack   []string   `json:"tech_stack"`
	Meta        MetaInfo    `json:"meta"`
}

type ImageInfo struct {
	Src  string `json:"src"`
	Alt  string `json:"alt"`
	Kind string `json:"kind"` // img | bg | og | icon | picture
}

type AudioInfo struct {
	Src   string `json:"src"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

type MetaInfo struct {
	Description string `json:"description"`
	OGImage     string `json:"og_image"`
	Favicon     string `json:"favicon"`
}

var (
	hexColorRe   = regexp.MustCompile(`#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})\b`)
	rgbColorRe   = regexp.MustCompile(`rgba?\(\s*\d{1,3}\s*,\s*\d{1,3}\s*,\s*\d{1,3}(?:\s*,\s*(?:0|1|0?\.\d+))?\s*\)`)
	hslColorRe   = regexp.MustCompile(`hsla?\(\s*\d{1,3}\s*,\s*\d{1,3}%\s*,\s*\d{1,3}%\s*(?:,\s*(?:0|1|0?\.\d+))?\s*\)`)
	fontFamilyRe = regexp.MustCompile(`font-family\s*:\s*([^;{}]+)`)
	bgURLRe      = regexp.MustCompile(`url\(\s*["']?([^"'\)]+)["']?\s*\)`)
	importRe     = regexp.MustCompile(`@import\s+(?:url\()?["']?([^"'\)]+)["']?\)?`)
)

func FetchStatic(targetURL string) (*FetchResult, error) {
	start := time.Now()
	client := &http.Client{Timeout: 25 * time.Second}
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("site returned %d %s", resp.StatusCode, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	html := string(body)
	text, title := cleanHTML(html)
	images, colors, colorCounts, fonts, sheets, structure, audio, framework, techStack, meta := extractSignals(html, targetURL)
	// Optionally enrich with external CSS (best-effort, short timeout)
	if len(sheets) > 0 {
		ext := fetchExternalCSS(sheets[:min(3, len(sheets))])
		if ext != "" {
			extraColors, extraCounts := extractColorsFromBlob(ext, colorCounts)
			colors = mergeColors(colors, extraCounts, extraColors)
			extraFonts := extractFontsFromBlob(ext)
			fonts = mergeFonts(fonts, extraFonts)
		}
	}
	return &FetchResult{
		HTML: html, Text: text, Title: title, URL: targetURL,
		Duration: time.Since(start), CharCount: len(text),
		Images: images, Colors: colors, ColorCounts: colorCounts, Fonts: fonts, Stylesheets: sheets, Structure: structure, Audio: audio,
		Framework: framework, TechStack: techStack, Meta: meta,
	}, nil
}

func FetchWithJS(targetURL string) (*FetchResult, error) {
	start := time.Now()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel2 := chromedp.NewContext(allocCtx)
	defer cancel2()
	ctx, cancel3 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel3()

	var html string
	// Try to grab computed design tokens via JS when possible
	var jsData struct {
		Colors []string `json:"colors"`
		Fonts  []string `json:"fonts"`
		Bgs    []string `json:"bgs"`
	}
	err := chromedp.Run(ctx,
		chromedp.Navigate(targetURL),
		chromedp.WaitVisible("body", chromedp.ByQuery),
		chromedp.Sleep(1800*time.Millisecond),
		chromedp.OuterHTML("html", &html),
		chromedp.Evaluate(`(() => {
			try {
				const els = Array.from(document.querySelectorAll('*')).slice(0,120);
				const cols = new Set();
				const fonts = new Set();
				const bgs = [];
				for (const el of els) {
					const cs = getComputedStyle(el);
					if (cs.color) cols.add(cs.color);
					if (cs.backgroundColor && cs.backgroundColor !== 'rgba(0, 0, 0, 0)' && cs.backgroundColor !== 'transparent') cols.add(cs.backgroundColor);
					if (cs.borderColor) cols.add(cs.borderColor);
					if (cs.fontFamily) fonts.add(cs.fontFamily.split(',')[0].replace(/["']/g,'').trim());
					if (cs.backgroundImage && cs.backgroundImage !== 'none') {
						const m = cs.backgroundImage.match(/url\(["']?([^"')]+)["']?\)/);
						if (m && m[1]) bgs.push(m[1]);
					}
				}
				return {colors: Array.from(cols).slice(0,24), fonts: Array.from(fonts).slice(0,12), bgs: bgs.slice(0,12)};
			} catch(e) { return {colors:[], fonts:[], bgs:[]}; }
		})()`, &jsData),
	)
	// Fallback if evaluate struct dance fails — just get HTML
	if err != nil {
		// retry without evaluate
		html = ""
		err = chromedp.Run(ctx,
			chromedp.Navigate(targetURL),
			chromedp.WaitVisible("body", chromedp.ByQuery),
			chromedp.Sleep(2*time.Second),
			chromedp.OuterHTML("html", &html),
		)
		if err != nil {
			return nil, fmt.Errorf("headless fetch failed: %w", err)
		}
	}
	text, title := cleanHTML(html)
	images, colors, colorCounts, fonts, sheets, structure, audio, framework, techStack, meta := extractSignals(html, targetURL)

	// Merge JS-computed signals
	if len(jsData.Colors) > 0 {
		for _, c := range jsData.Colors {
			c = strings.TrimSpace(c)
			if c == "" || c == "rgba(0, 0, 0, 0)" || c == "transparent" {
				continue
			}
			norm := strings.ToLower(c)
			if _, ok := colorCounts[norm]; !ok {
				colors = append(colors, norm)
				if colorCounts == nil {
					colorCounts = map[string]int{}
				}
				colorCounts[norm] = 1
			} else {
				colorCounts[norm]++
			}
		}
	}
	if len(jsData.Fonts) > 0 {
		fontSet := map[string]bool{}
		for _, f := range fonts {
			fontSet[f] = true
		}
		for _, f := range jsData.Fonts {
			f = strings.Trim(f, "\"' ")
			if f == "" || len(f) > 40 {
				continue
			}
			if !fontSet[f] {
				fonts = append(fonts, f)
				fontSet[f] = true
			}
		}
	}
	if len(jsData.Bgs) > 0 {
		base, _ := url.Parse(targetURL)
		seen := map[string]bool{}
		for _, im := range images {
			seen[im.Src] = true
		}
		for _, bg := range jsData.Bgs {
			if bg == "" || strings.HasPrefix(bg, "data:") {
				continue
			}
			if base != nil && !strings.HasPrefix(bg, "http") && !strings.HasPrefix(bg, "//") {
				if u, err := base.Parse(bg); err == nil {
					bg = u.String()
				}
			}
			if !seen[bg] {
				images = append(images, ImageInfo{Src: bg, Alt: "", Kind: "bg"})
				seen[bg] = true
			}
		}
	}
	if len(sheets) > 0 {
		ext := fetchExternalCSS(sheets[:min(3, len(sheets))])
		if ext != "" {
			extraColors, extraCounts := extractColorsFromBlob(ext, colorCounts)
			colors = mergeColors(colors, extraCounts, extraColors)
			extraFonts := extractFontsFromBlob(ext)
			fonts = mergeFonts(fonts, extraFonts)
		}
	}
	return &FetchResult{
		HTML: html, Text: text, Title: title, URL: targetURL,
		Duration: time.Since(start), CharCount: len(text),
		Images: images, Colors: colors, ColorCounts: colorCounts, Fonts: fonts, Stylesheets: sheets, Structure: structure, Audio: audio,
		Framework: framework, TechStack: techStack, Meta: meta,
	}, nil
}

func extractSignals(rawHTML, baseURL string) (images []ImageInfo, colors []string, colorCounts map[string]int, fonts []string, sheets []string, structure string, audio []AudioInfo, framework string, techStack []string, meta MetaInfo) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return nil, nil, nil, nil, nil, "", nil, "", nil, MetaInfo{}
	}
	base, _ := url.Parse(baseURL)
	seenImg := map[string]bool{}

	addImage := func(src, alt, kind string) {
		src = strings.TrimSpace(src)
		if src == "" || strings.HasPrefix(src, "data:") || strings.HasPrefix(src, "blob:") {
			return
		}
		// strip fragments
		if idx := strings.Index(src, "#"); idx != -1 {
			src = src[:idx]
		}
		// resolve relative
		if base != nil && !strings.HasPrefix(src, "http") && !strings.HasPrefix(src, "//") {
			if u, err := base.Parse(src); err == nil {
				src = u.String()
			}
		}
		// handle protocol-relative
		if strings.HasPrefix(src, "//") {
			src = "https:" + src
		}
		if seenImg[src] {
			return
		}
		seenImg[src] = true
		images = append(images, ImageInfo{Src: src, Alt: strings.TrimSpace(alt), Kind: kind})
	}

	// --- Images: comprehensive ---
	doc.Find("img").Each(func(_ int, s *goquery.Selection) {
		src, _ := s.Attr("src")
		if src == "" {
			src, _ = s.Attr("data-src")
		}
		if src == "" {
			src, _ = s.Attr("data-lazy-src")
		}
		if src == "" {
			src, _ = s.Attr("data-original")
		}
		// srcset: take first candidate
		if src == "" {
			if ss, ok := s.Attr("srcset"); ok && ss != "" {
				parts := strings.Split(ss, ",")
				if len(parts) > 0 {
					first := strings.TrimSpace(strings.Fields(strings.TrimSpace(parts[0]))[0])
					src = first
				}
			}
		}
		// also prefer srcset largest? we just use src
		if src == "" {
			if ss, ok := s.Attr("data-srcset"); ok && ss != "" {
				parts := strings.Split(ss, ",")
				if len(parts) > 0 {
					src = strings.TrimSpace(strings.Fields(strings.TrimSpace(parts[0]))[0])
				}
			}
		}
		alt, _ := s.Attr("alt")
		addImage(src, alt, "img")
		if len(images) >= 80 {
			return
		}
	})
	doc.Find("picture source[srcset], picture source[data-srcset]").Each(func(_ int, s *goquery.Selection) {
		ss, _ := s.Attr("srcset")
		if ss == "" {
			ss, _ = s.Attr("data-srcset")
		}
		for _, part := range strings.Split(ss, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			u := strings.Fields(part)[0]
			addImage(u, "", "picture")
			break // only first per source
		}
	})
	// OG / twitter images
	doc.Find(`meta[property="og:image"], meta[name="og:image"], meta[property="og:image:url"], meta[name="twitter:image"], meta[name="twitter:image:src"]`).Each(func(_ int, s *goquery.Selection) {
		if c, ok := s.Attr("content"); ok {
			addImage(c, "og:image", "og")
		}
	})
	// link icons / favicons (also images)
	var favicon string
	doc.Find(`link[rel="icon"], link[rel="shortcut icon"], link[rel="apple-touch-icon"]`).Each(func(_ int, s *goquery.Selection) {
		if href, ok := s.Attr("href"); ok {
			if favicon == "" {
				favicon = href
				if base != nil && !strings.HasPrefix(href, "http") && !strings.HasPrefix(href, "//") {
					if u, err := base.Parse(href); err == nil {
						favicon = u.String()
					}
				}
				if strings.HasPrefix(favicon, "//") {
					favicon = "https:" + favicon
				}
			}
			addImage(href, "icon", "icon")
		}
	})
	// CSS background images from style tags + inline styles
	var styleBlobs []string
	doc.Find("style").Each(func(_ int, s *goquery.Selection) {
		styleBlobs = append(styleBlobs, s.Text())
	})
	doc.Find("[style]").Each(func(_ int, s *goquery.Selection) {
		if v, ok := s.Attr("style"); ok {
			styleBlobs = append(styleBlobs, v)
		}
	})
	blob := strings.Join(styleBlobs, "\n")
	for _, m := range bgURLRe.FindAllStringSubmatch(blob, -1) {
		if len(m) < 2 {
			continue
		}
		u := strings.TrimSpace(m[1])
		if strings.Contains(u, ".woff") || strings.Contains(u, ".ttf") || strings.HasSuffix(u, ".css") {
			continue
		}
		// crude image extension check or allow any url that looks like image path
		lower := strings.ToLower(u)
		if strings.Contains(lower, ".jpg") || strings.Contains(lower, ".jpeg") || strings.Contains(lower, ".png") || strings.Contains(lower, ".webp") || strings.Contains(lower, ".gif") || strings.Contains(lower, ".svg") || strings.Contains(lower, ".avif") || strings.Contains(lower, "image") {
			addImage(u, "", "bg")
		} else if strings.HasPrefix(lower, "http") || strings.HasPrefix(lower, "/") {
			// still consider if it has image-like path segment already filtered, skip generic
		}
		if len(images) >= 80 {
			break
		}
	}
	// also background-image urls in SVG <image>
	doc.Find("svg image").Each(func(_ int, s *goquery.Selection) {
		if href, ok := s.Attr("href"); ok {
			addImage(href, "", "svg")
		}
		if href, ok := s.Attr("xlink:href"); ok {
			addImage(href, "", "svg")
		}
	})

	// Stylesheets
	doc.Find("link[rel='stylesheet']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		href = strings.TrimSpace(href)
		if href == "" {
			return
		}
		if base != nil && !strings.HasPrefix(href, "http") && !strings.HasPrefix(href, "//") {
			if u, err := base.Parse(href); err == nil {
				href = u.String()
			}
		}
		if strings.HasPrefix(href, "//") {
			href = "https:" + href
		}
		sheets = append(sheets, href)
		if len(sheets) >= 20 {
			return
		}
	})

	// Meta
	meta = MetaInfo{}
	if d, ok := doc.Find(`meta[name="description"]`).First().Attr("content"); ok {
		meta.Description = strings.TrimSpace(d)
	}
	if og, ok := doc.Find(`meta[property="og:image"]`).First().Attr("content"); ok {
		if base != nil && !strings.HasPrefix(og, "http") && !strings.HasPrefix(og, "//") {
			if u, err := base.Parse(og); err == nil {
				og = u.String()
			}
		}
		meta.OGImage = og
	}
	meta.Favicon = favicon

	// Colors and fonts from blobs + rawHTML
	colors, colorCounts = extractColorsFromBlob(blob+"\n"+rawHTML, nil)
	fonts = extractFontsFromBlob(blob + "\n" + rawHTML)
	// include @import fonts
	for _, m := range importRe.FindAllStringSubmatch(rawHTML, -1) {
		if len(m) >= 2 && strings.Contains(strings.ToLower(m[1]), "font") {
			// e.g. Google Fonts
			fonts = append(fonts, m[1])
		}
	}
	// detect Google Fonts links
	doc.Find(`link[href*="fonts.googleapis"]`).Each(func(_ int, s *goquery.Selection) {
		if href, ok := s.Attr("href"); ok {
			// parse family param
			if u, err := url.Parse(href); err == nil {
				if fam := u.Query().Get("family"); fam != "" {
					fam = strings.Split(fam, ":")[0]
					fam = strings.ReplaceAll(fam, "+", " ")
					fam = strings.Split(fam, "&")[0]
					if fam != "" && len(fam) < 40 {
						// avoid dup
						found := false
						for _, f := range fonts {
							if strings.EqualFold(f, fam) {
								found = true
								break
							}
						}
						if !found {
							fonts = append(fonts, fam)
						}
					}
				}
			}
		}
	})
	sort.Strings(fonts)
	// dedup fonts case-insensitive
	seenF := map[string]bool{}
	var dedupFonts []string
	for _, f := range fonts {
		k := strings.ToLower(strings.TrimSpace(f))
		if k == "" || seenF[k] {
			continue
		}
		seenF[k] = true
		dedupFonts = append(dedupFonts, f)
		if len(dedupFonts) >= 20 {
			break
		}
	}
	fonts = dedupFonts

	// Audio and music names
	doc.Find("audio, [data-audio], a[href$='.mp3'], a[href$='.wav'], a[href$='.flac'], a[href$='.m4a']").Each(func(_ int, s *goquery.Selection) {
		src, _ := s.Attr("src")
		if src == "" {
			src, _ = s.Attr("href")
		}
		if src == "" {
			src, _ = s.Attr("data-src")
		}
		src = strings.TrimSpace(src)
		title := strings.TrimSpace(s.Text())
		if t, ok := s.Attr("title"); ok && title == "" {
			title = strings.TrimSpace(t)
		}
		typ := goquery.NodeName(s)
		if src == "" && title == "" {
			return
		}
		if base != nil && src != "" && !strings.HasPrefix(src, "http") && !strings.HasPrefix(src, "//") {
			if u, err := base.Parse(src); err == nil {
				src = u.String()
			}
		}
		audio = append(audio, AudioInfo{Src: src, Title: title, Type: typ})
		if len(audio) >= 40 {
			return
		}
	})
	doc.Find("[class*='track' i], [class*='song' i], [class*='music' i], [id*='track' i], [id*='song' i]").Each(func(_ int, s *goquery.Selection) {
		t := strings.TrimSpace(s.Text())
		t = strings.Join(strings.Fields(t), " ")
		if len(t) < 3 || len(t) > 120 {
			return
		}
		if len(audio) >= 40 {
			return
		}
		for _, a := range audio {
			if a.Title == t {
				return
			}
		}
		audio = append(audio, AudioInfo{Src: "", Title: t, Type: "hint"})
	})

	// Framework detection
	lowerHTML := strings.ToLower(rawHTML)
	switch {
	case strings.Contains(lowerHTML, "tailwind"):
		framework = "Tailwind CSS"
	case strings.Contains(lowerHTML, "bootstrap"):
		framework = "Bootstrap"
	case strings.Contains(lowerHTML, "bulma"):
		framework = "Bulma"
	case strings.Contains(lowerHTML, "chakra"):
		framework = "Chakra UI"
	case strings.Contains(lowerHTML, "material"):
		framework = "Material"
	default:
		// heuristic on class names
		classBlob := ""
		doc.Find("[class]").Each(func(_ int, s *goquery.Selection) {
			if c, ok := s.Attr("class"); ok {
				classBlob += " " + c
			}
		})
		if strings.Contains(classBlob, "flex") && strings.Contains(classBlob, "grid") && strings.Contains(classBlob, "p-") {
			framework = "Tailwind CSS (likely)"
		} else if strings.Contains(classBlob, "container") && strings.Contains(classBlob, "row") && strings.Contains(classBlob, "col-") {
			framework = "Bootstrap (likely)"
		}
	}

	// Web structures: hierarchical outline
	var parts []string
	doc.Find("h1, h2, h3, h4").Each(func(_ int, s *goquery.Selection) {
		tag := goquery.NodeName(s)
		t := strings.TrimSpace(s.Text())
		t = strings.Join(strings.Fields(t), " ")
		if t == "" || len(t) > 120 {
			return
		}
		indent := ""
		switch tag {
		case "h2":
			indent = "  "
		case "h3":
			indent = "    "
		case "h4":
			indent = "      "
		}
		parts = append(parts, indent+strings.ToUpper(tag)+": "+t)
		if len(parts) >= 50 {
			return
		}
	})
	var landmarks []string
	for _, sel := range []string{"header", "nav", "main", "section", "article", "aside", "footer"} {
		n := doc.Find(sel).Length()
		if n > 0 {
			landmarks = append(landmarks, fmt.Sprintf("<%s> x%d", sel, n))
		}
	}
	if len(landmarks) > 0 {
		parts = append(parts, "— Landmarks: "+strings.Join(landmarks, " · "))
	}
	// count images/sections hint
	parts = append(parts, fmt.Sprintf("— Images found: %d · Stylesheets: %d · Colors: %d · Fonts: %d", len(images), len(sheets), len(colors), len(fonts)))
	if framework != "" {
		parts = append(parts, "— Framework: "+framework)
	}
	structure = strings.Join(parts, "\n")
	if len(structure) > 1800 {
		structure = structure[:1800] + "..."
	}
	techStack = detectTechStack(doc, rawHTML, lowerHTML, sheets, framework)
	return images, colors, colorCounts, fonts, sheets, structure, audio, framework, techStack, meta
}


func detectTechStack(doc *goquery.Document, rawHTML, lowerHTML string, sheets []string, framework string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	contains := func(sub string) bool { return strings.Contains(lowerHTML, sub) }
	// JS frameworks
	if contains("__next_data__") || contains("_next/static") || contains("next/image") {
		add("Next.js")
	}
	if contains("react") && (contains("react-dom") || contains("react.development") || contains("__react")) {
		add("React")
	} else if contains("react") && contains("next") {
		add("React")
	}
	if contains("vue") || contains("nuxt") {
		if contains("nuxt") { add("Nuxt") } else { add("Vue.js") }
	}
	if contains("angular") { add("Angular") }
	if contains("svelte") { add("Svelte") }
	if contains("astro") { add("Astro") }
	if contains("remix") { add("Remix") }
	// CSS
	if framework != "" { add(framework) }
	if contains("tailwind") { add("Tailwind CSS") }
	if contains("bootstrap") { add("Bootstrap") }
	if contains("bulma") { add("Bulma") }
	if contains("chakra") { add("Chakra UI") }
	// CMS / site builders
	if contains("wp-content") || contains("wp-includes") || contains("wordpress") { add("WordPress") }
	if contains("shopify") || contains("cdn.shopify") { add("Shopify") }
	if contains("webflow") { add("Webflow") }
	if contains("wix.com") || contains("wixstatic") { add("Wix") }
	if contains("squarespace") { add("Squarespace") }
	if contains("contentful") { add("Contentful") }
	if contains("sanity.io") { add("Sanity") }
	// Libraries
	if contains("jquery") { add("jQuery") }
	if contains("gsap") { add("GSAP") }
	if contains("framer") { add("Framer Motion") }
	if contains("three.js") || contains("threejs") { add("Three.js") }
	if contains("gsap") { add("GSAP") }
	// Analytics / marketing
	if contains("googletagmanager") || contains("google-analytics") || contains("gtag(") { add("Google Analytics") }
	if contains("googletagmanager.com/gtm") || contains("gtm.js") { add("Google Tag Manager") }
	if contains("hotjar") { add("Hotjar") }
	if contains("segment.com") || contains("segment.io") { add("Segment") }
	if contains("mixpanel") { add("Mixpanel") }
	if contains("facebook.net/tr") || contains("connect.facebook.net") { add("Meta Pixel") }
	// Payments / commerce
	if contains("stripe") { add("Stripe") }
	if contains("paypal") { add("PayPal") }
	// Infra / CDN
	if contains("cloudflare") { add("Cloudflare") }
	if contains("vercel") { add("Vercel") }
	if contains("netlify") { add("Netlify") }
	if contains("__cf") { add("Cloudflare") }
	// Build
	if contains("vite") { add("Vite") }
	if contains("webpack") { add("Webpack") }
	// Check script/link sources more precisely
	doc.Find("script[src], link[href]").Each(func(_ int, sel *goquery.Selection) {
		src, _ := sel.Attr("src")
		if src == "" { src, _ = sel.Attr("href") }
		low := strings.ToLower(src)
		if strings.Contains(low, "next/static") && !seen["Next.js"] { add("Next.js") }
		if strings.Contains(low, "wp-content") && !seen["WordPress"] { add("WordPress") }
		if strings.Contains(low, "shopify") && !seen["Shopify"] { add("Shopify") }
		if strings.Contains(low, "googletagmanager") && !seen["Google Analytics"] { add("Google Analytics") }
	})
	// Stylesheet hints
	for _, sh := range sheets {
		low := strings.ToLower(sh)
		if strings.Contains(low, "tailwind") && !seen["Tailwind CSS"] { add("Tailwind CSS") }
		if strings.Contains(low, "bootstrap") && !seen["Bootstrap"] { add("Bootstrap") }
	}
	if len(out) == 0 {
		out = append(out, "HTML/CSS/JS")
	}
	if len(out) > 14 {
		out = out[:14]
	}
	return out
}

func extractColorsFromBlob(blob string, existing map[string]int) ([]string, map[string]int) {
	counts := map[string]int{}
	if existing != nil {
		for k, v := range existing {
			counts[k] = v
		}
	}
	for _, m := range hexColorRe.FindAllString(blob, -1) {
		c := strings.ToLower(m)
		// normalize #abc -> #aabbcc for counting consistency? keep short but treat same? expand for dedup
		if len(c) == 4 {
			c = fmt.Sprintf("#%c%c%c%c%c%c", c[1], c[1], c[2], c[2], c[3], c[3])
		}
		if len(c) == 9 { // #rrggbbaa truncate alpha for palette
			c = c[:7]
		}
		// skip pure black/white extremes if overrepresented? keep all but de-prioritize later — but keep
		counts[c]++
	}
	for _, m := range rgbColorRe.FindAllString(blob, -1) {
		c := strings.ToLower(strings.Join(strings.Fields(m), ""))
		// normalize spacing
		c = strings.ReplaceAll(c, ",,", ",")
		counts[c]++
	}
	for _, m := range hslColorRe.FindAllString(blob, -1) {
		c := strings.ToLower(strings.Join(strings.Fields(m), ""))
		counts[c]++
	}
	// sort by frequency desc
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range counts {
		// filter obvious noise: transparent etc — already not matched
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].v == kvs[j].v {
			return kvs[i].k < kvs[j].k
		}
		return kvs[i].v > kvs[j].v
	})
	var colors []string
	for _, p := range kvs {
		colors = append(colors, p.k)
		if len(colors) >= 32 {
			break
		}
	}
	return colors, counts
}

func extractFontsFromBlob(blob string) []string {
	set := map[string]bool{}
	var out []string
	for _, m := range fontFamilyRe.FindAllStringSubmatch(blob, -1) {
		if len(m) < 2 {
			continue
		}
		f := strings.TrimSpace(m[1])
		f = strings.Trim(f, "\"' ")
		if idx := strings.Index(f, ","); idx != -1 {
			f = strings.TrimSpace(f[:idx])
		}
		f = strings.Trim(f, "\"' ")
		if f == "" || len(f) > 40 {
			continue
		}
		// skip generic families alone
		low := strings.ToLower(f)
		if low == "sans-serif" || low == "serif" || low == "monospace" || low == "cursive" || low == "fantasy" || low == "system-ui" {
			continue
		}
		if !set[f] {
			set[f] = true
			out = append(out, f)
		}
	}
	return out
}

func mergeColors(existing []string, counts map[string]int, extra []string) []string {
	// rebuild sorted from counts
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range counts {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].v == kvs[j].v {
			return kvs[i].k < kvs[j].k
		}
		return kvs[i].v > kvs[j].v
	})
	var out []string
	for _, p := range kvs {
		out = append(out, p.k)
		if len(out) >= 32 {
			break
		}
	}
	_ = extra
	return out
}

func mergeFonts(a, b []string) []string {
	set := map[string]bool{}
	var out []string
	for _, f := range a {
		k := strings.ToLower(f)
		if !set[k] {
			set[k] = true
			out = append(out, f)
		}
	}
	for _, f := range b {
		k := strings.ToLower(strings.TrimSpace(f))
		if k == "" || set[k] {
			continue
		}
		set[k] = true
		out = append(out, f)
	}
	sort.Strings(out)
	// dedup preserve sorted but cap
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func fetchExternalCSS(urls []string) string {
	client := &http.Client{Timeout: 6 * time.Second}
	var sb strings.Builder
	for _, u := range urls {
		if !strings.HasSuffix(strings.ToLower(u), ".css") && !strings.Contains(u, ".css") {
			continue
		}
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 300<<10))
		resp.Body.Close()
		if err != nil {
			continue
		}
		sb.Write(body)
		sb.WriteString("\n")
		if sb.Len() > 400<<10 {
			break
		}
	}
	return sb.String()
}

func cleanHTML(raw string) (string, string) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(raw))
	if err != nil {
		return truncate(raw, 15000), ""
	}
	title := strings.TrimSpace(doc.Find("title").First().Text())
	doc.Find("script, style, noscript, iframe, svg, canvas").Remove()
	doc.Find("nav, footer, header").Remove()

	var text string
	if m := doc.Find("main"); m.Length() > 0 {
		text = extractBlockText(m.First())
	} else if s := doc.Find("section"); s.Length() > 0 && len(strings.Fields(s.First().Text())) > 80 {
		text = extractBlockText(s.First())
	} else {
		text = extractBlockText(doc.Find("body"))
	}
	if len(strings.Fields(text)) < 40 {
		text = extractBlockText(doc.Find("body"))
	}
	if strings.TrimSpace(text) == "" {
		text = strings.Join(strings.Fields(doc.Text()), " ")
	}
	text = truncate(text, 12000)
	return text, title
}

func extractBlockText(sel *goquery.Selection) string {
	var parts []string
	seen := map[string]bool{}
	sel.Find("h1, h2, h3, h4, p, li, article, tr, dt, dd, .product_pod, .quote").Each(func(_ int, s *goquery.Selection) {
		t := strings.TrimSpace(s.Text())
		t = strings.Join(strings.Fields(t), " ")
		if len(t) < 12 || len(t) > 600 {
			return
		}
		if seen[t] {
			return
		}
		seen[t] = true
		parts = append(parts, t)
	})
	if len(parts) >= 3 {
		joined := strings.Join(parts, "\n")
		if len(joined) > 500 {
			return joined
		}
	}
	txt := strings.TrimSpace(sel.Text())
	txt = strings.Join(strings.Fields(txt), " ")
	return txt
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
