package scraper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// AuditResult is the full audit attached to an Analyze run.
type AuditResult struct {
	OverallScore int                  `json:"overall_score"`
	OverallLabel string               `json:"overall_label"`
	Categories   []AuditCategory      `json:"categories"`
	TopFixes     []AuditRecommendation `json:"top_fixes"`
	GeneratedBy  string               `json:"generated_by"` // heuristic | heuristic+ai
}

type AuditCategory struct {
	Key       string      `json:"key"`   // seo | performance | accessibility | security | ux | content
	Label     string      `json:"label"`
	Score     int         `json:"score"`
	Status    string      `json:"status"` // good | warn | bad
	Issues    []AuditIssue `json:"issues"`
	Strengths []string    `json:"strengths"`
}

type AuditIssue struct {
	Severity string `json:"severity"` // high | medium | low
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

type AuditRecommendation struct {
	Priority    int    `json:"priority"`
	Category    string `json:"category"`
	Title       string `json:"title"`
	Why         string `json:"why"`
	Fix         string `json:"fix"`
	CodeSnippet string `json:"code_snippet,omitempty"`
	Impact      string `json:"impact"` // high | medium | low
	Effort      string `json:"effort"` // low | medium | high
}

// ComputeHeuristicAudit runs deterministic checks from FetchResult signals.
// Always succeeds (no network, no AI). Used as baseline and fallback.
func ComputeHeuristicAudit(fetch *FetchResult) *AuditResult {
	if fetch == nil {
		return &AuditResult{OverallScore: 0, OverallLabel: "No data", GeneratedBy: "heuristic"}
	}
	html := fetch.HTML
	lower := strings.ToLower(html)
	doc, _ := goquery.NewDocumentFromReader(strings.NewReader(html))

	cats := []AuditCategory{
		scoreSEO(fetch, doc, lower),
		scorePerformance(fetch, doc, lower),
		scoreA11y(fetch, doc, lower),
		scoreSecurity(fetch, doc, lower),
		scoreUX(fetch, doc, lower),
		scoreContent(fetch, doc, lower),
	}
	overall := 0
	for _, c := range cats {
		overall += c.Score
	}
	if len(cats) > 0 {
		overall = overall / len(cats)
	}
	label := overallLabel(overall)
	top := synthesizeTopFixes(cats, 10)

	return &AuditResult{
		OverallScore: clamp(overall, 0, 100),
		OverallLabel: label,
		Categories:   cats,
		TopFixes:     top,
		GeneratedBy:  "heuristic",
	}
}

func overallLabel(s int) string {
	switch {
	case s >= 85:
		return "Excellent"
	case s >= 70:
		return "Good"
	case s >= 50:
		return "Needs work"
	default:
		return "Poor"
	}
}
func statusFor(s int) string {
	if s >= 80 {
		return "good"
	}
	if s >= 50 {
		return "warn"
	}
	return "bad"
}
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------- category scorers ----------

func scoreSEO(fetch *FetchResult, doc *goquery.Document, lower string) AuditCategory {
	score := 100
	var issues []AuditIssue
	var strengths []string

	title := strings.TrimSpace(fetch.Title)
	if title == "" {
		score -= 35
		issues = append(issues, AuditIssue{"high", "Missing <title>", "Page has no title tag. Search engines and social previews will be blank.",})
	} else {
		l := len([]rune(title))
		if l < 20 {
			score -= 15
			issues = append(issues, AuditIssue{"medium", "Title too short", fmt.Sprintf("Title is %d chars. Aim for 30-60 chars for SERP display. Current: %q", l, title)})
		} else if l > 65 {
			score -= 10
			issues = append(issues, AuditIssue{"low", "Title too long", fmt.Sprintf("Title is %d chars and will truncate in search results. Trim to ~60 chars.", l)})
		} else {
			strengths = append(strengths, "Title length looks good")
		}
	}
	desc := strings.TrimSpace(fetch.Meta.Description)
	if desc == "" {
		score -= 25
		issues = append(issues, AuditIssue{"high", "Missing meta description", "No meta description found. Add a 120-160 char summary for search snippets."})
	} else {
		l := len([]rune(desc))
		if l < 70 {
			score -= 12
			issues = append(issues, AuditIssue{"medium", "Meta description too short", fmt.Sprintf("%d chars. Expand to 120-160 for better CTR.", l)})
		} else if l > 170 {
			score -= 8
			issues = append(issues, AuditIssue{"low", "Meta description too long", fmt.Sprintf("%d chars. Trim to ~155 so it doesn't truncate.", l)})
		} else {
			strengths = append(strengths, "Meta description present")
		}
	}
	// H1 checks
	h1n := 0
	if doc != nil {
		h1n = doc.Find("h1").Length()
	}
	if h1n == 0 {
		score -= 18
		issues = append(issues, AuditIssue{"high", "No H1 found", "Add a single descriptive H1 that matches the title intent."})
	} else if h1n > 1 {
		score -= 10
		issues = append(issues, AuditIssue{"medium", fmt.Sprintf("%d H1 tags", h1n), "Use one H1 per page; demote extras to H2/H3 for correct outline."})
	} else {
		strengths = append(strengths, "Single H1 present")
	}
	// heading hierarchy skip
	if doc != nil {
		hasH1, hasH2, hasH3 := doc.Find("h1").Length() > 0, doc.Find("h2").Length() > 0, doc.Find("h3").Length() > 0
		if hasH3 && !hasH2 {
			score -= 8
			issues = append(issues, AuditIssue{"low", "Skipped heading level", "H3 appears without H2. Keep hierarchy H1 -> H2 -> H3."})
		}
		if hasH1 && hasH2 {
			strengths = append(strengths, "Heading hierarchy present")
		}
	}
	if fetch.Meta.OGImage == "" {
		score -= 8
		issues = append(issues, AuditIssue{"low", "No Open Graph image", "Add og:image (1200x630) so links unfurl with a preview."})
	} else {
		strengths = append(strengths, "OG image present")
	}
	if fetch.Meta.Favicon == "" {
		score -= 4
		issues = append(issues, AuditIssue{"low", "No favicon detected", "Add a favicon and apple-touch-icon."})
	}
	if !strings.Contains(lower, `rel="canonical"`) && !strings.Contains(lower, `rel='canonical'`) {
		score -= 6
		issues = append(issues, AuditIssue{"low", "No canonical tag", "Add <link rel=\"canonical\"> to avoid duplicate-content issues."})
	}
	if fetch.Text != "" {
		wc := len(strings.Fields(fetch.Text))
		if wc < 200 {
			score -= 8
			issues = append(issues, AuditIssue{"low", "Thin content", fmt.Sprintf("Only ~%d words detected. Add more substantive copy for SEO.", wc)})
		} else if wc > 600 {
			strengths = append(strengths, fmt.Sprintf("Substantial copy (~%d words)", wc))
		}
	}
	score = clamp(score, 0, 100)
	return AuditCategory{Key: "seo", Label: "SEO", Score: score, Status: statusFor(score), Issues: issues, Strengths: strengths}
}

func scorePerformance(fetch *FetchResult, doc *goquery.Document, lower string) AuditCategory {
	score := 100
	var issues []AuditIssue
	var strengths []string

	htmlSize := len(fetch.HTML)
	if htmlSize > 600*1024 {
		score -= 18
		issues = append(issues, AuditIssue{"medium", "Large HTML payload", fmt.Sprintf("HTML is ~%d KB. Consider code-splitting and removing unused CSS/JS.", htmlSize/1024)})
	} else if htmlSize > 250*1024 {
		score -= 8
		issues = append(issues, AuditIssue{"low", "HTML above 250 KB", "Trim inline scripts/styles and defer non-critical JS."})
	} else {
		strengths = append(strengths, fmt.Sprintf("HTML size reasonable (~%d KB)", htmlSize/1024))
	}
	if n := len(fetch.Images); n > 40 {
		score -= 18
		issues = append(issues, AuditIssue{"medium", fmt.Sprintf("%d images detected", n), "Many images. Lazy-load, use modern formats (AVIF/WebP), and serve responsive sizes."})
	} else if n > 20 {
		score -= 8
		issues = append(issues, AuditIssue{"low", fmt.Sprintf("%d images", n), "Consider lazy-loading below-the-fold images."})
	} else if n > 0 {
		strengths = append(strengths, fmt.Sprintf("%d images", n))
	}
	if n := len(fetch.Stylesheets); n > 7 {
		score -= 14
		issues = append(issues, AuditIssue{"medium", fmt.Sprintf("%d stylesheets", n), "Consolidate CSS and remove unused styles. Inline critical CSS."})
	} else if n > 4 {
		score -= 6
		issues = append(issues, AuditIssue{"low", fmt.Sprintf("%d stylesheets", n), "Reduce to 2-4 CSS files for fewer round trips."})
	}
	// lazy loading
	if doc != nil {
		totalImg := doc.Find("img").Length()
		lazyImg := doc.Find("img[loading='lazy'], img[loading=\"lazy\"]").Length()
		if totalImg >= 6 && lazyImg == 0 {
			score -= 10
			issues = append(issues, AuditIssue{"low", "No lazy-loading", "Add loading=\"lazy\" to below-the-fold images."})
		} else if lazyImg > 0 {
			strengths = append(strengths, "Lazy-loading in use")
		}
		// render-blocking hint
		if doc.Find("script[src]").Length() > 6 {
			score -= 8
			issues = append(issues, AuditIssue{"low", "Many external scripts", "Defer non-critical scripts and remove unused JS."})
		}
	}
	if strings.Contains(lower, "google fonts") || strings.Contains(lower, "fonts.googleapis") {
		if len(fetch.Fonts) > 3 {
			score -= 6
			issues = append(issues, AuditIssue{"low", fmt.Sprintf("%d web fonts", len(fetch.Fonts)), "Limit to 1-2 families; use font-display:swap."})
		}
	}
	// compression hint
	if htmlSize > 0 && len(fetch.Text) > 0 {
		ratio := float64(len(fetch.Text)) / float64(htmlSize)
		if ratio < 0.12 {
			score -= 6
			issues = append(issues, AuditIssue{"low", "Low text-to-HTML ratio", "Much markup vs content may indicate heavy frameworks. Check bundle size."})
		}
	}
	score = clamp(score, 0, 100)
	return AuditCategory{Key: "performance", Label: "Performance", Score: score, Status: statusFor(score), Issues: issues, Strengths: strengths}
}

func scoreA11y(fetch *FetchResult, doc *goquery.Document, lower string) AuditCategory {
	score := 100
	var issues []AuditIssue
	var strengths []string

	if doc != nil {
		totalImg := doc.Find("img").Length()
		missingAlt := 0
		doc.Find("img").Each(func(_ int, s *goquery.Selection) {
			alt, _ := s.Attr("alt")
			if strings.TrimSpace(alt) == "" {
				missingAlt++
			}
		})
		if totalImg > 0 && missingAlt > 0 {
			pen := missingAlt * 6
			if pen > 24 {
				pen = 24
			}
			score -= pen
			sev := "medium"
			if missingAlt > 4 {
				sev = "high"
			}
			issues = append(issues, AuditIssue{sev, fmt.Sprintf("%d/%d images missing alt", missingAlt, totalImg), "Add descriptive alt text. Decorative images should have alt=\"\" explicitly."})
		} else if totalImg > 0 {
			strengths = append(strengths, "Images have alt text")
		}
		// lang attr
		hasLang := false
		if doc.Find("html[lang]").Length() > 0 {
			hasLang = true
		}
		if !hasLang {
			score -= 12
			issues = append(issues, AuditIssue{"medium", "Missing html lang", "Add <html lang=\"en\"> for screen readers and translation."})
		} else {
			strengths = append(strengths, "Language declared")
		}
		// landmarks
		landmarks := 0
		for _, sel := range []string{"header", "nav", "main", "footer"} {
			if doc.Find(sel).Length() > 0 {
				landmarks++
			}
		}
		if landmarks < 2 {
			score -= 10
			issues = append(issues, AuditIssue{"medium", "Few landmarks", "Use <header>, <nav>, <main>, <footer> for screen reader navigation."})
		} else {
			strengths = append(strengths, "Semantic landmarks present")
		}
		// form labels
		inputs := doc.Find("input:not([type='hidden'])").Length()
		labels := doc.Find("label").Length()
		if inputs > 0 && labels == 0 {
			score -= 10
			issues = append(issues, AuditIssue{"medium", "Inputs without labels", "Associate inputs with <label> or aria-label for accessibility."})
		}
		// skip link
		if !strings.Contains(lower, "skip") {
			// low severity hint, not penalize heavily
		}
		// heading order already partly in SEO but add here too
		h1 := doc.Find("h1").Length()
		h2 := doc.Find("h2").Length()
		if h1 == 0 {
			score -= 8
			issues = append(issues, AuditIssue{"medium", "No H1 for outline", "Screen readers rely on H1 as page title."})
		}
		if h2 == 0 && doc.Find("h3").Length() > 0 {
			score -= 6
			issues = append(issues, AuditIssue{"low", "Heading levels skipped", "Maintain sequential heading order."})
		}
	}
	score = clamp(score, 0, 100)
	return AuditCategory{Key: "accessibility", Label: "Accessibility", Score: score, Status: statusFor(score), Issues: issues, Strengths: strengths}
}

func scoreSecurity(fetch *FetchResult, doc *goquery.Document, lower string) AuditCategory {
	score := 100
	var issues []AuditIssue
	var strengths []string

	u, err := url.Parse(fetch.URL)
	if err == nil {
		if u.Scheme == "http" {
			score -= 35
			issues = append(issues, AuditIssue{"high", "Not HTTPS", "Serve over HTTPS with a valid certificate and redirect HTTP -> HTTPS."})
		} else if u.Scheme == "https" {
			strengths = append(strengths, "Served over HTTPS")
		}
	}
	// mixed content
	if fetch.URL != "" && strings.HasPrefix(strings.ToLower(fetch.URL), "https://") {
		mixed := 0
		if doc != nil {
			doc.Find("img[src^='http://'], script[src^='http://'], link[href^='http://']").Each(func(_ int, _ *goquery.Selection) { mixed++ })
		}
		if mixed > 0 {
			score -= 12
			issues = append(issues, AuditIssue{"medium", "Mixed content", fmt.Sprintf("%d sub-resources loaded over http:// on an https page. Switch to https.", mixed)})
		} else if doc != nil {
			strengths = append(strengths, "No mixed content detected")
		}
	}
	// external links without rel
	if doc != nil {
		externalNoRel := 0
		totalExternal := 0
		doc.Find("a[href^='http']").Each(func(_ int, s *goquery.Selection) {
			href, _ := s.Attr("href")
			if href == "" {
				return
			}
			if u != nil && strings.Contains(href, u.Host) {
				return
			}
			totalExternal++
			rel, _ := s.Attr("rel")
			if !strings.Contains(strings.ToLower(rel), "noopener") {
				externalNoRel++
			}
		})
		if totalExternal > 0 && externalNoRel > 0 && externalNoRel*2 >= totalExternal {
			score -= 8
			issues = append(issues, AuditIssue{"low", "External links without rel=noopener", "Add rel=\"noopener noreferrer\" to target=\"_blank\" externals."})
		}
	}
	if strings.Contains(lower, "content-security-policy") {
		strengths = append(strengths, "CSP hint present")
	} else {
		issues = append(issues, AuditIssue{"low", "No CSP detected", "Consider Content-Security-Policy headers to mitigate XSS."})
		score -= 4
	}
	score = clamp(score, 0, 100)
	return AuditCategory{Key: "security", Label: "Security", Score: score, Status: statusFor(score), Issues: issues, Strengths: strengths}
}

func scoreUX(fetch *FetchResult, doc *goquery.Document, lower string) AuditCategory {
	score := 100
	var issues []AuditIssue
	var strengths []string

	// viewport
	if !strings.Contains(lower, "viewport") {
		score -= 18
		issues = append(issues, AuditIssue{"high", "Missing viewport meta", "Add <meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">."})
	} else {
		strengths = append(strengths, "Viewport meta present")
	}
	// palette
	if len(fetch.Colors) == 0 {
		score -= 6
		issues = append(issues, AuditIssue{"low", "No palette detected", "Palette may be image-based or CSS-variable driven."})
	} else if len(fetch.Colors) > 14 {
		score -= 6
		issues = append(issues, AuditIssue{"low", "Many colors", fmt.Sprintf("%d colors — consider consolidating to 4-6 core tokens.", len(fetch.Colors))})
	} else {
		strengths = append(strengths, fmt.Sprintf("Palette of %d colors", len(fetch.Colors)))
	}
	if len(fetch.Fonts) > 4 {
		score -= 10
		issues = append(issues, AuditIssue{"medium", fmt.Sprintf("%d font families", len(fetch.Fonts)), "Limit to 2 families (heading + body) for hierarchy."})
	} else if len(fetch.Fonts) >= 1 && len(fetch.Fonts) <= 3 {
		strengths = append(strengths, "Font stack focused")
	}
	// CTA / flows proxy
	ctaCount := 0
	if doc != nil {
		doc.Find("a, button").Each(func(_ int, s *goquery.Selection) {
			t := strings.TrimSpace(strings.ToLower(s.Text()))
			if t == "" {
				t, _ = s.Attr("aria-label")
				t = strings.ToLower(t)
			}
			for _, kw := range []string{"get started", "sign up", "try", "buy", "shop", "contact", "learn more", "subscribe", "download", "book", "request"} {
				if strings.Contains(t, kw) {
					ctaCount++
					break
				}
			}
		})
	}
	if ctaCount == 0 {
		score -= 14
		issues = append(issues, AuditIssue{"medium", "No clear CTA detected", "Add a primary CTA (e.g. Get Started) above the fold and in nav."})
	} else if ctaCount == 1 {
		issues = append(issues, AuditIssue{"low", "Single CTA", "Consider a secondary CTA (Learn more) to capture lower-intent visitors."})
		score -= 4
	} else {
		strengths = append(strengths, fmt.Sprintf("%d CTAs detected", ctaCount))
	}
	// nav
	hasNav := false
	if doc != nil && doc.Find("nav, [role='navigation']").Length() > 0 {
		hasNav = true
	}
	if !hasNav {
		score -= 12
		issues = append(issues, AuditIssue{"medium", "No <nav> detected", "Add a semantic nav for primary navigation."})
	} else {
		strengths = append(strengths, "Navigation present")
	}
	// touch targets hint
	if doc != nil {
		links := doc.Find("a, button").Length()
		if links > 60 {
			issues = append(issues, AuditIssue{"low", "Many interactive elements", "Ensure 44px minimum touch targets on mobile."})
			score -= 4
		}
	}
	score = clamp(score, 0, 100)
	return AuditCategory{Key: "ux", Label: "UX / Design", Score: score, Status: statusFor(score), Issues: issues, Strengths: strengths}
}

var seoWordRe = regexp.MustCompile(`[A-Za-z]{3,}`)

func scoreContent(fetch *FetchResult, doc *goquery.Document, lower string) AuditCategory {
	score := 100
	var issues []AuditIssue
	var strengths []string

	wc := len(strings.Fields(fetch.Text))
	if wc < 150 {
		score -= 20
		issues = append(issues, AuditIssue{"medium", "Very little text", fmt.Sprintf("~%d words. Add benefit-led copy, FAQs, and social proof.", wc)})
	} else if wc < 350 {
		score -= 10
		issues = append(issues, AuditIssue{"low", "Short page", fmt.Sprintf("~%d words. Expand key sections with proof and detail.", wc)})
	} else if wc > 800 {
		strengths = append(strengths, fmt.Sprintf("Rich copy (~%d words)", wc))
	}
	// structure depth
	h2n, h3n := 0, 0
	if doc != nil {
		h2n = doc.Find("h2").Length()
		h3n = doc.Find("h3").Length()
	}
	if h2n == 0 {
		score -= 12
		issues = append(issues, AuditIssue{"medium", "No H2 sections", "Break content into H2 sections (Features, Pricing, FAQ) for scannability."})
	} else if h2n >= 3 {
		strengths = append(strengths, fmt.Sprintf("%d sections", h2n))
	}
	if wc > 300 && h2n > 0 && h3n == 0 {
		issues = append(issues, AuditIssue{"low", "No H3 subsections", "Add H3s inside long sections to improve scan."})
		score -= 4
	}
	// CTA vs content balance
	if doc != nil {
		ctaN := 0
		doc.Find("a, button").Each(func(_ int, s *goquery.Selection) {
			t := strings.ToLower(strings.TrimSpace(s.Text()))
			if strings.Contains(t, "get") || strings.Contains(t, "try") || strings.Contains(t, "sign") || strings.Contains(t, "buy") || strings.Contains(t, "contact") {
				ctaN++
			}
		})
		if ctaN > 0 && wc > 0 {
			ratio := float64(ctaN) / float64(wc) * 1000
			if ratio < 1.0 {
				issues = append(issues, AuditIssue{"low", "Few CTAs per content", "Add contextual CTAs inside sections, not just hero."})
				score -= 4
			}
		}
	}
	// proof signals
	hasProof := strings.Contains(lower, "testimonial") || strings.Contains(lower, "trusted by") || strings.Contains(lower, "customers") || strings.Contains(lower, "reviews")
	if !hasProof && wc > 250 {
		issues = append(issues, AuditIssue{"low", "No social proof detected", "Add logos, testimonials, or stats to build trust."})
		score -= 6
	} else if hasProof {
		strengths = append(strengths, "Social proof signals found")
	}
	// readability: avg word len proxy
	if wc > 50 {
		letters := 0
		for _, w := range strings.Fields(fetch.Text) {
			if seoWordRe.MatchString(w) {
				letters += len(w)
			}
		}
		avg := float64(letters) / float64(wc)
		if avg > 5.6 {
			issues = append(issues, AuditIssue{"low", "Copy skews complex", "Shorten sentences and favor plain language for wider comprehension."})
			score -= 4
		}
	}
	score = clamp(score, 0, 100)
	return AuditCategory{Key: "content", Label: "Content / Conversion", Score: score, Status: statusFor(score), Issues: issues, Strengths: strengths}
}

func synthesizeTopFixes(cats []AuditCategory, n int) []AuditRecommendation {
	type scored struct {
		rec   AuditRecommendation
		score int // lower = higher priority (high severity + low effort first)
	}
	var cands []scored
	pri := 1
	for _, c := range cats {
		for _, is := range c.Issues {
			impact := "medium"
			effort := "medium"
			if is.Severity == "high" {
				impact = "high"
				effort = "low"
			}
			if c.Key == "performance" && strings.Contains(is.Title, "images") {
				effort = "low"
			}
			if c.Key == "seo" && strings.Contains(is.Title, "title") {
				effort = "low"
			}
			rec := AuditRecommendation{
				Priority: pri,
				Category: c.Label,
				Title:    is.Title,
				Why:      is.Detail,
				Fix:      fixFor(c.Key, is),
				Impact:   impact,
				Effort:   effort,
			}
			// code snippets for actionable fixes
			rec.CodeSnippet = codeFor(c.Key, is)
			sevWeight := 0
			if is.Severity == "high" {
				sevWeight = 0
			} else if is.Severity == "medium" {
				sevWeight = 10
			} else {
				sevWeight = 20
			}
			effWeight := 0
			if effort == "low" {
				effWeight = 0
			} else if effort == "medium" {
				effWeight = 5
			} else {
				effWeight = 10
			}
			cands = append(cands, scored{rec, sevWeight + effWeight})
			pri++
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score < cands[j].score })
	var out []AuditRecommendation
	for i, c := range cands {
		if i >= n {
			break
		}
		c.rec.Priority = i + 1
		out = append(out, c.rec)
	}
	return out
}

func fixFor(cat string, is AuditIssue) string {
	switch is.Title {
	case "Missing <title>":
		return "Add <title>Your Brand — clear benefit in 50-60 chars</title> in <head>."
	case "Missing meta description":
		return "Add <meta name=\"description\" content=\"Benefit-led 140-char summary with primary keyword.\">"
	case "No H1 found":
		return "Add one H1 matching user intent, e.g. <h1>Ship faster with ScrapeAI</h1>."
	case "Missing html lang":
		return "Set <html lang=\"en\"> (or appropriate locale)."
	case "Missing viewport meta":
		return "Add <meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">."
	case "Not HTTPS":
		return "Issue TLS cert (Let's Encrypt), enforce 301 http→https and HSTS."
	case "No lazy-loading":
		return "Add loading=\"lazy\" and width/height to below-the-fold <img>."
	case "Few landmarks":
		return "Wrap sections in <header>, <nav>, <main>, <footer> and use <section> with headings."
	case "Inputs without labels":
		return "Add <label for=\"id\"> or aria-label to each input."
	case "Missing OG image":
		return "Add <meta property=\"og:image\" content=\"https://.../og-1200x630.jpg\">."
	default:
		if strings.Contains(is.Title, "images missing alt") {
			return "Add descriptive alt to each <img>; decorative images: alt=\"\"."
		}
		if strings.Contains(is.Title, "stylesheets") {
			return "Bundle CSS, remove unused rules (PurgeCSS), inline critical CSS."
		}
		if strings.Contains(is.Title, "Large HTML") {
			return "Code-split JS, remove unused CSS/JS, defer non-critical scripts."
		}
		return is.Detail
	}
}

func codeFor(cat string, is AuditIssue) string {
	switch is.Title {
	case "Missing <title>":
		return "<title>ScrapeAI — Extract data and rebuild any site from a URL</title>"
	case "Missing meta description":
		return "<meta name=\"description\" content=\"Paste any URL to extract tables, colors, fonts, and rebuild prompts. Built for makers shipping fast.\">"
	case "No H1 found":
		return "<h1>Analyze any site. Get rebuild prompts.</h1>"
	case "Missing html lang":
		return "<html lang=\"en\">"
	case "Missing viewport meta":
		return "<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">"
	case "No lazy-loading":
		return "<img src=\"/hero.webp\" alt=\"Dashboard\" loading=\"lazy\" width=\"1200\" height=\"630\">"
	case "External links without rel=noopener":
		return "<a href=\"https://example.com\" target=\"_blank\" rel=\"noopener noreferrer\">External</a>"
	case "Missing OG image":
		return "<meta property=\"og:image\" content=\"https://example.com/og-1200x630.jpg\">\n<meta property=\"og:image:width\" content=\"1200\">"
	default:
		return ""
	}
}

// ---------- AI enhancement: prioritized recommendations ----------

type auditAIResponse struct {
	Recommendations []AuditRecommendation `json:"recommendations"`
}

// GenerateAuditRecommendations calls Groq to enrich heuristic audit with prioritized fixes.
// If GROQ_API_KEY is missing or call fails, it returns the heuristic audit unchanged (no error).
func GenerateAuditRecommendations(fetch *FetchResult, heuristic *AuditResult) (*AuditResult, string, error) {
	apiKey := os.Getenv("GROQ_API_KEY")
	if apiKey == "" {
		pretty, _ := json.MarshalIndent(heuristic, "", "  ")
		return heuristic, string(pretty), nil
	}
	model := os.Getenv("GROQ_MODEL")
	if model == "" {
		model = "qwen/qwen3-32b"
	}
	if model == "qwen/qwen3.8-27b" || model == "qwen/qwen3-27b" {
		model = "qwen/qwen3-32b"
	}
	systemPrompt := `You are a senior web auditor (SEO, performance, a11y, security, UX, conversion). Given a site's scraped signals and a heuristic audit (scores + issues), produce 8-12 prioritized recommendations to increase traffic, enhance quality, and fix defects.

Return ONLY valid JSON with this exact shape (no markdown fences, no commentary):
{
  "recommendations": [
    {
      "category": "SEO | Performance | Accessibility | Security | UX / Design | Content / Conversion",
      "title": "concise fix title",
      "why": "1 sentence why it matters (user/business impact)",
      "fix": "concrete steps to fix (2-3 sentences, actionable)",
      "code_snippet": "optional minimal HTML/CSS/JS snippet or empty string",
      "impact": "high|medium|low",
      "effort": "low|medium|high"
    }
  ]
}

Rules:
- Order by priority: high impact + low effort first.
- Be specific to signals (use actual title length, image counts, framework hints). Don't invent URLs not in signals.
- Cover at least 4 different categories.
- Keep title <= 60 chars, why/fix concise.
- code_snippet only when a small snippet helps (meta tag, html attr, css). Otherwise "".
`

	signals := formatSignals(fetch)
	heuristicJSON, _ := json.MarshalIndent(heuristic, "", "  ")
	userContent := fmt.Sprintf("URL: %s\nTitle: %s\n\nSignals:\n%s\n\nPage text (truncated):\n%s\n\nHeuristic audit JSON:\n%s\n\nGenerate recommendations JSON now.", fetch.URL, fetch.Title, signals, truncateRunes(fetch.Text, 4000), string(heuristicJSON))

	reqBody := GroqRequest{
		Model:       model,
		Temperature: 0.25,
		MaxTokens:   3500,
		Messages: []GroqMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
	}
	fallbacks := []string{"llama-3.3-70b-versatile", "llama-3.1-8b-instant", "openai/gpt-oss-120b", "qwen/qwen3.8-27b"}
	modelsToTry := []string{model}
	for _, fb := range fallbacks {
		if fb != model {
			modelsToTry = append(modelsToTry, fb)
		}
	}
	client := &http.Client{Timeout: 60 * time.Second}
	var body []byte
	var status int
	var lastErr string
	var grBody []byte
	tried := ""
ModelLoop:
	for _, m := range modelsToTry {
		reqBody.Model = m
		payload, _ := json.Marshal(reqBody)
		var headers http.Header
		for attempt := 0; attempt < 3; attempt++ {
			req, err := http.NewRequest("POST", "https://api.groq.com/openai/v1/chat/completions", bytes.NewReader(payload))
			if err != nil {
				return heuristic, "", err
			}
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				return heuristic, "", fmt.Errorf("groq request failed: %w", err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			status = resp.StatusCode
			headers = resp.Header
			body = b
			grBody = b
			if status == 429 && attempt < 2 {
				wait := time.Duration(2+attempt*4) * time.Second
				if ra := headers.Get("Retry-After"); ra != "" {
					if d, err := time.ParseDuration(ra + "s"); err == nil {
						wait = d + 500*time.Millisecond
					}
				}
				time.Sleep(wait)
				continue
			}
			break
		}
		tried += m + " "
		if status == 200 {
			break ModelLoop
		}
		bs := string(body)
		isModelNotFound := status == 404 && (strings.Contains(bs, "model_not_found") || strings.Contains(bs, "does not exist"))
		if isModelNotFound {
			lastErr = fmt.Sprintf("groq %d (%s): %s", status, m, bs)
			continue
		}
		// auth or other errors: fall back to heuristic silently (don't break analyze)
		pretty, _ := json.MarshalIndent(heuristic, "", "  ")
		return heuristic, string(pretty), fmt.Errorf("groq %d (%s): %s (tried: %s)", status, m, bs, strings.TrimSpace(tried))
	}
	if status != 200 {
		pretty, _ := json.MarshalIndent(heuristic, "", "  ")
		if lastErr != "" {
			return heuristic, string(pretty), fmt.Errorf("%s (tried: %s)", lastErr, strings.TrimSpace(tried))
		}
		return heuristic, string(pretty), fmt.Errorf("groq %d: %s", status, string(grBody))
	}
	body = grBody
	var gr GroqResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		pretty, _ := json.MarshalIndent(heuristic, "", "  ")
		return heuristic, string(pretty), fmt.Errorf("groq parse error: %w", err)
	}
	if len(gr.Choices) == 0 {
		pretty, _ := json.MarshalIndent(heuristic, "", "  ")
		return heuristic, string(pretty), fmt.Errorf("groq returned no choices")
	}
	raw := strings.TrimSpace(gr.Choices[0].Message.Content)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var aiResp auditAIResponse
	if err := json.Unmarshal([]byte(raw), &aiResp); err != nil {
		start := strings.Index(raw, "{")
		end := strings.LastIndex(raw, "}")
		if start != -1 && end != -1 && end > start {
			sub := raw[start : end+1]
			if err2 := json.Unmarshal([]byte(sub), &aiResp); err2 != nil {
				pretty, _ := json.MarshalIndent(heuristic, "", "  ")
				return heuristic, string(pretty), fmt.Errorf("audit model returned non-JSON: %w", err)
			}
		} else {
			pretty, _ := json.MarshalIndent(heuristic, "", "  ")
			return heuristic, string(pretty), fmt.Errorf("audit model returned non-JSON: %w", err)
		}
	}
	// Map AI recommendations back onto audit, overriding TopFixes, keeping scores from heuristic.
	// Normalize priorities and clamp counts.
	var recs []AuditRecommendation
	for i, r := range aiResp.Recommendations {
		if strings.TrimSpace(r.Title) == "" {
			continue
		}
		r.Priority = i + 1
		if r.Impact == "" {
			r.Impact = "medium"
		}
		if r.Effort == "" {
			r.Effort = "medium"
		}
		// normalize category label
		if r.Category == "" {
			r.Category = "General"
		}
		recs = append(recs, r)
		if len(recs) >= 12 {
			break
		}
	}
	if len(recs) >= 4 {
		heuristic.TopFixes = recs
		heuristic.GeneratedBy = "heuristic+ai"
	}
	pretty, _ := json.MarshalIndent(heuristic, "", "  ")
	return heuristic, string(pretty), nil
}
