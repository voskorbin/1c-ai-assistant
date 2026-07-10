package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// confluenceCacheEntry хранит закэшированное содержимое страницы.
type confluenceCacheEntry struct {
	content   MCPResourceContent
	fetchedAt time.Time
}

// ConfluenceFetcher загружает содержимое страниц Confluence по URL.
type ConfluenceFetcher struct {
	client   *http.Client
	baseURL  string
	token    string
	cache    map[string]confluenceCacheEntry
	cacheMu  sync.RWMutex
	cacheTTL time.Duration
}

// NewConfluenceFetcher создаёт fetcher из переменных окружения.
func NewConfluenceFetcher() *ConfluenceFetcher {
	return &ConfluenceFetcher{
		client:   &http.Client{Timeout: 30 * time.Second},
		baseURL:  strings.TrimRight(os.Getenv("CONFLUENCE_URL"), "/"),
		token:    os.Getenv("CONFLUENCE_PERSONAL_TOKEN"),
		cache:    make(map[string]confluenceCacheEntry),
		cacheTTL: 5 * time.Minute,
	}
}

// FetchPages загружает указанные страницы Confluence с кэшированием.
func (c *ConfluenceFetcher) FetchPages(urls []string) ([]MCPResourceContent, error) {
	if c.baseURL == "" || c.token == "" {
		return nil, fmt.Errorf("CONFLUENCE_URL or CONFLUENCE_PERSONAL_TOKEN not set")
	}

	start := time.Now()
	result := make([]MCPResourceContent, 0, len(urls))
	for _, pageURL := range urls {
		c.cacheMu.RLock()
		entry, ok := c.cache[pageURL]
		c.cacheMu.RUnlock()
		if ok && time.Since(entry.fetchedAt) < c.cacheTTL {
			result = append(result, entry.content)
			continue
		}

		pageStart := time.Now()
		content, err := c.fetchPage(pageURL)
		if err != nil {
			log.Printf("[ConfluenceFetcher] failed to fetch %s: %v (took %v)", pageURL, err, time.Since(pageStart))
			continue
		}
		log.Printf("[ConfluenceFetcher] fetched %s in %v", pageURL, time.Since(pageStart))

		c.cacheMu.Lock()
		c.cache[pageURL] = confluenceCacheEntry{content: content, fetchedAt: time.Now()}
		c.cacheMu.Unlock()
		result = append(result, content)
	}
	log.Printf("[ConfluenceFetcher] FetchPages total: %d urls, %v", len(urls), time.Since(start))

	return result, nil
}

func (c *ConfluenceFetcher) fetchPage(pageURL string) (MCPResourceContent, error) {
	pageID, spaceKey, title, err := c.parseConfluenceURL(pageURL)
	if err != nil {
		return MCPResourceContent{}, err
	}

	var apiURL string
	if pageID != "" {
		apiURL = fmt.Sprintf("%s/rest/api/content/%s?expand=body.storage,title", c.baseURL, pageID)
	} else {
		apiURL = fmt.Sprintf("%s/rest/api/content?title=%s&spaceKey=%s&expand=body.storage,title",
			c.baseURL, url.QueryEscape(title), url.QueryEscape(spaceKey))
	}

	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return MCPResourceContent{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return MCPResourceContent{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return MCPResourceContent{}, err
	}

	if resp.StatusCode != http.StatusOK {
		return MCPResourceContent{}, fmt.Errorf("confluence API status %d: %s", resp.StatusCode, string(body))
	}

	return c.extractContent(pageURL, body)
}

func (c *ConfluenceFetcher) parseConfluenceURL(pageURL string) (pageID, spaceKey, title string, err error) {
	parsed, err := url.Parse(pageURL)
	if err != nil {
		return "", "", "", err
	}

	path := parsed.Path

	// Формат /pages/viewpage.action?pageId=...
	if strings.Contains(path, "/pages/viewpage.action") {
		pageID = parsed.Query().Get("pageId")
		if pageID == "" {
			return "", "", "", fmt.Errorf("missing pageId parameter")
		}
		return pageID, "", "", nil
	}

	// Формат /display/<space>/<title>
	if strings.HasPrefix(path, "/display/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/display/"), "/", 2)
		if len(parts) != 2 {
			return "", "", "", fmt.Errorf("invalid display URL")
		}
		spaceKey = parts[0]
		title = strings.ReplaceAll(parts[1], "+", " ")
		title, err = url.PathUnescape(title)
		if err != nil {
			return "", "", "", err
		}
		return "", spaceKey, title, nil
	}

	return "", "", "", fmt.Errorf("unsupported Confluence URL format")
}

func (c *ConfluenceFetcher) extractContent(pageURL string, body []byte) (MCPResourceContent, error) {
	var pageData struct {
		Title string `json:"title"`
		Body  struct {
			Storage struct {
				Value string `json:"value"`
			} `json:"storage"`
		} `json:"body"`
		Results []struct {
			Title string `json:"title"`
			Body  struct {
				Storage struct {
					Value string `json:"value"`
				} `json:"storage"`
			} `json:"body"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &pageData); err != nil {
		return MCPResourceContent{}, err
	}

	title := pageData.Title
	content := pageData.Body.Storage.Value

	if title == "" && len(pageData.Results) > 0 {
		title = pageData.Results[0].Title
		content = pageData.Results[0].Body.Storage.Value
	}

	if title == "" {
		title = pageURL
	}

	return MCPResourceContent{
		URI:  pageURL,
		Text: fmt.Sprintf("# %s\n\n%s", title, htmlToText(content)),
		Type: "text",
	}, nil
}

var htmlTagRegex = regexp.MustCompile("<[^>]*>")

func htmlToText(input string) string {
	// Заменяем <br>, <p> и т.п. на переносы строк для читаемости.
	text := strings.ReplaceAll(input, "</p>", "\n")
	text = strings.ReplaceAll(text, "</div>", "\n")
	text = strings.ReplaceAll(text, "<br/>", "\n")
	text = strings.ReplaceAll(text, "<br />", "\n")
	text = htmlTagRegex.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	return strings.TrimSpace(text)
}
