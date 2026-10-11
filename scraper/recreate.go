package scraper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// RecreationOutput is the structured response from the LLM for site analysis + rebuild prompts.
type RecreationOutput struct {
	Summary      string           `json:"summary"`
	SiteType     string           `json:"site_type"`
	Structure    StructureAnalysis `json:"structure"`
	Flows        []Flow           `json:"flows"`
	DesignTokens DesignTokens     `json:"design_tokens"`
	Prompts      []RecreatePrompt `json:"prompts"`
}

type StructureAnalysis struct {
	Layout      string   `json:"layout"`
	Sections    []string `json:"sections"`
	Navigation  string   `json:"navigation"`
	Headings    string   `json:"headings_outline"`
	FrameworkHint string `json:"framework_hint"`
}

type Flow struct {
	Name  string `json:"name"`
	Steps []string `json:"steps"`
	CTA   string `json:"cta"`
}

type DesignTokens struct {
	Palette     []string `json:"palette"`
	PaletteUsage string  `json:"palette_usage"`
	Typography  string   `json:"typography"`
	Styling     string   `json:"styling"`
	Vibe        string   `json:"vibe"`
}

type RecreatePrompt struct {
	Title  string `json:"title"`
	Target string `json:"target"` // e.g. "v0 / Lovable", "Next.js + Tailwind", "Figma"
	Prompt string `json:"prompt"`
}

func GenerateRecreationPrompts(fetch *FetchResult) (*RecreationOutput, string, error) {
	apiKey := os.Getenv("GROQ_API_KEY")
	if apiKey == "" {
		return nil, "", fmt.Errorf("GROQ_API_KEY not set")
	}
	model := os.Getenv("GROQ_MODEL")
	if model == "" {
		model = "qwen/qwen3-32b"
	}
	// migrate legacy ids; keep qwen/qwen3-32b as primary (fallback handled below on 404)
	if model == "qwen/qwen3.8-27b" || model == "qwen/qwen3-27b" {
		model = "qwen/qwen3-32b"
	}

	systemPrompt := `You are a senior product designer + frontend architect. Given a scraped website's signals (text, colors, fonts, images, structure, meta, stylesheets, framework hints), analyze the site and produce recreation prompts.

Return ONLY valid JSON with this exact shape (no markdown fences, no commentary):
{
  "summary": "1-2 sentence what the site is and its purpose",
  "site_type": "e.g. SaaS landing, ecommerce storefront, blog, portfolio, dashboard",
  "structure": {
    "layout": "overall layout pattern e.g. hero + 3-column features + pricing + footer",
    "sections": ["hero", "social proof", "features", "pricing", "faq", "footer"],
    "navigation": "nav structure and IA",
    "headings_outline": "h1/h2 outline as text",
    "framework_hint": "likely stack vibe e.g. Tailwind, minimal, glassmorphism"
  },
  "flows": [
    {"name": "Primary CTA flow", "steps": ["land on hero", "click CTA", "sign up"], "cta": "Get Started button in hero + nav"}
  ],
  "design_tokens": {
    "palette": ["#hex", "#hex"],
    "palette_usage": "how colors are used e.g. dark bg #09090B, accent teal #14B8A6 for CTAs",
    "typography": "font stack and heading/body treatment",
    "styling": "spacing, radius, shadows, border treatment, component style",
    "vibe": "aesthetic in 3-5 words e.g. minimal dark, editorial, playful neobrutalist"
  },
  "prompts": [
    {"title": "v0 / Lovable / Bolt — one-shot", "target": "v0", "prompt": "full copy-paste prompt to recreate this site in v0/Lovable/Bolt... include layout, palette hex codes, fonts, sections, flows, and content placeholders"},
    {"title": "Next.js + Tailwind — developer", "target": "code", "prompt": "detailed build prompt for a developer using Next.js + Tailwind... include component breakdown and Tailwind token suggestions"},
    {"title": "Figma — designer", "target": "figma", "prompt": "prompt to recreate as a Figma design... include auto-layout, tokens, and page structure"}
  ]
}

Rules:
- prompts[].prompt must be long, detailed, copy-paste ready (150-300 words each). Include actual hex colors, font names, and section names from signals. Don't hallucinate images — reference counts.
- design_tokens.palette should mirror the detected palette (reuse hex values from signals when present).
- flows: 2-4 flows max. Infer from nav + headings + CTAs.
- Be specific and faithful to signals; don't invent brand names not in text/title.`

	signals := formatSignals(fetch)
	// also include a short domain hint from URL
	userContent := fmt.Sprintf("URL: %s\nTitle: %s\n\nSignals:\n%s\n\nPage text (truncated):\n%s", fetch.URL, fetch.Title, signals, truncateRunes(fetch.Text, 6000))

	reqBody := GroqRequest{
		Model:       model,
		Temperature: 0.3,
		MaxTokens:   4000,
		Messages: []GroqMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
	}
	// Build ordered model list: primary + fallbacks (avoids 422 when primary not entitled)
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
				return nil, "", err
			}
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				return nil, "", fmt.Errorf("groq request failed: %w", err)
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
		isModelNotFound := status == 404 && (strings.Contains(bs, "model_not_found") || strings.Contains(bs, "does not exist") || strings.Contains(bs, "model_not_found"))
		if isModelNotFound {
			lastErr = fmt.Sprintf("groq %d (%s): %s", status, m, bs)
			continue
		}
		// non-model error: surface directly
		return nil, "", fmt.Errorf("groq %d (%s): %s", status, m, bs)
	}
	if status != 200 {
		if lastErr != "" {
			return nil, "", fmt.Errorf("%s (tried: %s)", lastErr, strings.TrimSpace(tried))
		}
		return nil, "", fmt.Errorf("groq %d: %s", status, string(grBody))
	}
	body = grBody
	var gr GroqResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, "", fmt.Errorf("groq parse error: %w", err)
	}
	if len(gr.Choices) == 0 {
		return nil, "", fmt.Errorf("groq returned no choices")
	}
	raw := strings.TrimSpace(gr.Choices[0].Message.Content)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var out RecreationOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// try to extract JSON object
		start := strings.Index(raw, "{")
		end := strings.LastIndex(raw, "}")
		if start != -1 && end != -1 && end > start {
			sub := raw[start : end+1]
			if err2 := json.Unmarshal([]byte(sub), &out); err2 == nil {
				pretty, _ := json.MarshalIndent(out, "", "  ")
				return &out, string(pretty), nil
			}
		}
		return nil, raw, fmt.Errorf("model returned non-JSON: %w. raw: %.600s", err, raw)
	}
	pretty, _ := json.MarshalIndent(out, "", "  ")
	return &out, string(pretty), nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n...[truncated]"
}
