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

type GroqMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type GroqRequest struct {
	Model       string        `json:"model"`
	Messages    []GroqMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format,omitempty"`
}

type GroqResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func ExtractWithGroq(pageText, pageTitle, pageURL, userPrompt string) (interface{}, string, error) {
	fetch := &FetchResult{Text: pageText, Title: pageTitle, URL: pageURL}
	return ExtractWithGroqFromFetch(fetch, userPrompt)
}

func ExtractWithGroqFromFetch(fetch *FetchResult, userPrompt string) (interface{}, string, error) {
	apiKey := os.Getenv("GROQ_API_KEY")
	if apiKey == "" {
		return nil, "", fmt.Errorf("GROQ_API_KEY not set. Add it to .env")
	}
	model := os.Getenv("GROQ_MODEL")
	if model == "" {
		model = "qwen/qwen3.8-27b"
	}
	if model == "llama-3.3-70b-versatile" || model == "llama-3.1-8b-instant" || model == "qwen/qwen3-32b" {
		model = "qwen/qwen3.8-27b"
	}
	systemPrompt := "You are an expert web scraping extractor. Given cleaned page text plus unrestricted page signals and a user's instruction, return ONLY valid JSON.\n\nRules:\n- Output MUST be a JSON array of objects. Each object is one item.\n- If nothing matches, return [].\n- Keys should be snake_case inferred from the user's instruction.\n- Keep values concise. Prices as strings with currency if present.\n- Do NOT wrap in markdown fences. Do NOT add explanation. ONLY the JSON array.\n- No restriction on domain: web structures, headings, music or track or song names, image names alts and URLs, styling, color palettes, fonts, layouts. Extract exactly what the user asks without filtering."
	signals := formatSignals(fetch)
	userContent := fmt.Sprintf("URL: %s\nTitle: %s\n\nUser instruction: %s\n\nPage text:\n%s\n\nUnrestricted page signals (use when relevant to the prompt: images, audio tracks, colors, fonts, stylesheets, heading structure):\n%s", fetch.URL, fetch.Title, userPrompt, fetch.Text, signals)
	reqBody := GroqRequest{
		Model:       model,
		Temperature: 0.1,
		MaxTokens:   4000,
		Messages: []GroqMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
	}
	payload, _ := json.Marshal(reqBody)
	client := &http.Client{Timeout: 45 * time.Second}
	var body []byte
	var status int
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
	if status != 200 {
		return nil, "", fmt.Errorf("groq %d: %s", status, string(body))
	}
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
	var parsed interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		start := strings.Index(raw, "[")
		end := strings.LastIndex(raw, "]")
		if start != -1 && end != -1 && end > start {
			sub := raw[start : end+1]
			if err2 := json.Unmarshal([]byte(sub), &parsed); err2 == nil {
				pretty, _ := json.MarshalIndent(parsed, "", "  ")
				return parsed, string(pretty), nil
			}
		}
		return nil, raw, fmt.Errorf("model returned non JSON: %w. raw: %.500s", err, raw)
	}
	pretty, _ := json.MarshalIndent(parsed, "", "  ")
	return parsed, string(pretty), nil
}

func formatSignals(f *FetchResult) string {
	if f == nil {
		return "(none)"
	}
	var b strings.Builder
	if len(f.Images) > 0 {
		b.WriteString(fmt.Sprintf("Images (%d):\n", len(f.Images)))
		limit := 20
		if len(f.Images) < limit {
			limit = len(f.Images)
		}
		for i := 0; i < limit; i++ {
			im := f.Images[i]
			if im.Alt != "" {
				b.WriteString(fmt.Sprintf("- %s | alt: %s\n", im.Src, im.Alt))
			} else {
				b.WriteString(fmt.Sprintf("- %s\n", im.Src))
			}
		}
		if len(f.Images) > limit {
			b.WriteString(fmt.Sprintf("... and %d more\n", len(f.Images)-limit))
		}
	} else {
		b.WriteString("Images: none found\n")
	}
	if len(f.Audio) > 0 {
		b.WriteString(fmt.Sprintf("Audio and track hints (%d):\n", len(f.Audio)))
		limit := 20
		if len(f.Audio) < limit {
			limit = len(f.Audio)
		}
		for i := 0; i < limit; i++ {
			a := f.Audio[i]
			if a.Src != "" && a.Title != "" {
				b.WriteString(fmt.Sprintf("- [%s] %s -> %s\n", a.Type, a.Title, a.Src))
			} else if a.Title != "" {
				b.WriteString(fmt.Sprintf("- [%s] %s\n", a.Type, a.Title))
			} else {
				b.WriteString(fmt.Sprintf("- [%s] %s\n", a.Type, a.Src))
			}
		}
		if len(f.Audio) > limit {
			b.WriteString(fmt.Sprintf("... and %d more\n", len(f.Audio)-limit))
		}
	} else {
		b.WriteString("Audio: none found\n")
	}
	if len(f.Colors) > 0 {
		b.WriteString(fmt.Sprintf("Color palette (%d): %s\n", len(f.Colors), strings.Join(f.Colors, ", ")))
	} else {
		b.WriteString("Colors: none detected\n")
	}
	if len(f.Fonts) > 0 {
		b.WriteString(fmt.Sprintf("Fonts: %s\n", strings.Join(f.Fonts, ", ")))
	} else {
		b.WriteString("Fonts: none detected\n")
	}
	if len(f.Stylesheets) > 0 {
		limit := 8
		if len(f.Stylesheets) < limit {
			limit = len(f.Stylesheets)
		}
		b.WriteString(fmt.Sprintf("Stylesheets (%d): %s\n", len(f.Stylesheets), strings.Join(f.Stylesheets[:limit], ", ")))
	}
	if len(f.TechStack) > 0 {
		b.WriteString(fmt.Sprintf("Technologies: %s\n", strings.Join(f.TechStack, ", ")))
	}
	if f.Framework != "" {
		b.WriteString(fmt.Sprintf("Framework: %s\n", f.Framework))
	}
	if f.Meta.Description != "" {
		desc := f.Meta.Description
		if len(desc) > 240 { desc = desc[:240] + "…" }
		b.WriteString(fmt.Sprintf("Meta description: %s\n", desc))
	}
	if len(f.Stylesheets) > 6 {
		// already limited above, just ensure count shown
	}
	if f.Structure != "" {
		b.WriteString(fmt.Sprintf("Structure: %s\n", f.Structure))
	}
	s := b.String()
	if len(s) > 4000 {
		return s[:4000] + "\n...[signals truncated]"
	}
	return s
}
