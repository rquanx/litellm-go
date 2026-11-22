package proxyconfig

import (
	_ "embed"
	"encoding/xml"
	"io"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed public-blog-posts.json.gz
var publicBlogBackup []byte

type publicBlogPost struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Date        string `json:"date"`
	URL         string `json:"url"`
}

type publicBlogFeed struct {
	url    string
	client *http.Client
	mu     sync.Mutex
	post   *publicBlogPost
	expiry time.Time
}

func newPublicBlogFeed() http.Handler {
	if strings.EqualFold(os.Getenv("LITELLM_LOCAL_BLOG_POSTS"), "true") {
		return servePublicCatalog(publicBlogBackup)
	}
	url, set := os.LookupEnv("LITELLM_BLOG_POSTS_URL")
	if !set {
		url = "https://docs.litellm.ai/blog/rss.xml"
	}
	return &publicBlogFeed{url: url, client: &http.Client{Timeout: 5 * time.Second}}
}

func (feed *publicBlogFeed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	feed.mu.Lock()
	if time.Now().After(feed.expiry) {
		feed.post = feed.fetch()
		ttl := time.Hour
		if feed.post == nil {
			ttl = time.Minute
		}
		feed.expiry = time.Now().Add(ttl)
	}
	post := feed.post
	feed.mu.Unlock()
	if post == nil {
		servePublicCatalog(publicBlogBackup)(w, r)
		return
	}
	modelJSON(w, http.StatusOK, map[string][]publicBlogPost{"posts": {*post}})
}

func (feed *publicBlogFeed) fetch() *publicBlogPost {
	response, err := feed.client.Get(feed.url)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	var rss struct {
		Items []struct {
			Title       *string `xml:"title"`
			Description string  `xml:"description"`
			Date        string  `xml:"pubDate"`
			Link        *string `xml:"link"`
		} `xml:"channel>item"`
	}
	if err := xml.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&rss); err != nil {
		return nil
	}
	for _, item := range rss.Items {
		if item.Title == nil || item.Link == nil {
			continue
		}
		date := item.Date
		if parsed, err := mail.ParseDate(date); err == nil {
			date = parsed.Format("2006-01-02")
		}
		return &publicBlogPost{Title: *item.Title, Description: item.Description, Date: date, URL: *item.Link}
	}
	return nil
}
