package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"

	"github.com/antchfx/htmlquery"
	"github.com/antchfx/xpath"
	"github.com/mitchellh/mapstructure"
	"golang.org/x/net/html"
)

const (
	startChapter = 1
	endChapter   = 16
	worker       = 5
)

type Picture struct {
	URL     string
	Caption string
	ID      string
}

func main() {
	var chapters []int
	for i := startChapter; i <= endChapter; i++ {
		chapters = append(chapters, i)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	urls := generateGalleryURLs(ctx, chapters)
	pics := downloadGalleryHTML(ctx, urls)

	var wg sync.WaitGroup
	wg.Add(worker)
	for i := 0; i < worker; i++ {
		downloadPic(ctx, &wg, pics)
	}
	wg.Wait()
}

func generateGalleryURLs(ctx context.Context, chapters []int) <-chan string {
	const (
		urlConcept  = "https://www.starwars.com/series/the-mandalorian/chapter-%d-concept-art-gallery"
		urlConcept2 = "https://www.starwars.com/chapter-%d-concept-art-gallery"
		// urlStory   = "https://www.starwars.com/series/the-mandalorian/chapter-%d-story-gallery"
		// urlTrivia  = "https://www.starwars.com/series/the-mandalorian/chapter-%d-trivia-gallery"
	)

	urls := make(chan string, 3)
	go func() {
		defer close(urls)
		for _, chap := range chapters {
			select {
			case <-ctx.Done():
				return
			default:
			}

			urls <- fmt.Sprintf(urlConcept, chap)
			urls <- fmt.Sprintf(urlConcept2, chap)
		}
	}()
	return urls
}

func downloadGalleryHTML(ctx context.Context, urls <-chan string) (picURLs <-chan Picture) {
	picChan := make(chan Picture, 10)
	go func() {
		defer close(picChan)
		for url := range urls {
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				log.Printf("error creating request: %v", err)
				continue
			}
			err = httpDo(ctx, req, func(resp *http.Response, err error) error {
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				doc, err := html.Parse(resp.Body)
				if err != nil {
					return err
				}
				pics, err := parseForPic(doc)
				if err != nil {
					return err
				}
				for _, pic := range pics {
					picChan <- pic
				}
				return nil
			})
			if err != nil {
				log.Printf("error downloading gallery html: %v on %s", err, url)
				continue
			}
		}
	}()
	return picChan
}

var (
	picDataXpath   = xpath.MustCompile("//div[@id='main']/script")
	notFoundXpath  = xpath.MustCompile("//div[@id='main']/article[@id='error_page']")
	picDataPattern = regexp.MustCompile(`this\.Grill\?Grill\.burger=(.*):\(function\(\)`)
)

func parseForPic(doc *html.Node) ([]Picture, error) {
	scriptNode := htmlquery.QuerySelector(doc, picDataXpath)

	if scriptNode == nil || scriptNode.FirstChild == nil {
		notFound := htmlquery.QuerySelector(doc, notFoundXpath)
		if notFound != nil {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot find html node for pictures")
	}

	captures := picDataPattern.FindSubmatch([]byte(scriptNode.FirstChild.Data))
	if len(captures) < 2 {
		return nil, fmt.Errorf("unable to find regex match")
	}

	var m map[string]interface{}
	err := json.Unmarshal(captures[1], &m)
	if err != nil {
		return nil, err
	}

	var data struct {
		Stack []struct {
			Data []struct {
				Images []struct {
					Image   string `mapstructure:"image"`
					Caption string `mapstructure:"caption"`
					ID      string `mapstructure:"id"`
				} `mapstructure:"images"`
			} `mapstructure:"data"`
		} `mapstructure:"stack"`
	}
	err = mapstructure.Decode(m, &data)
	if err != nil {
		return nil, err
	}

	var pics []Picture
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("panic: %w", e)
		}
	}()
	for _, p := range data.Stack[2].Data[0].Images {
		pics = append(pics, Picture{
			URL:     p.Image,
			Caption: p.Caption,
			ID:      p.ID,
		})
	}
	return pics, nil
}

func downloadPic(ctx context.Context, wg *sync.WaitGroup, pics <-chan Picture) {
	defer wg.Done()

	const downloadDir = "download"
	if err := os.Mkdir(downloadDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		log.Printf("unable to create download directory: %v", err)
		return
	}
	for p := range pics {
		select {
		case <-ctx.Done():
			return
		default:
		}
		func() {
			if len(p.Caption) > 64 {
				p.Caption = p.Caption[:64+1]
			}
			fname := fmt.Sprintf("download%c%s_%s.jpeg", os.PathSeparator, p.Caption, p.ID)
			f, err := os.Create(fname)
			if err != nil {
				log.Printf("unable to create file: %v", err)
				return
			}
			defer f.Close()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
			if err != nil {
				log.Printf("unable to create download request: %v", err)
				return
			}
			err = httpDo(ctx, req, func(resp *http.Response, err error) error {
				if err != nil {
					return err
				}
				defer resp.Body.Close()

				_, err = io.Copy(f, resp.Body)
				if err != nil {
					return err
				}
				log.Printf("downloaded %v", fname)
				return nil
			})
			if err != nil {
				log.Printf("unable to download file: %v", err)
			}
		}()
	}
}

// httpDo makes an HTTP request. It passes the HTTP response to closure f for it to handle.
func httpDo(ctx context.Context, req *http.Request, f func(*http.Response, error) error) error {
	c := make(chan error, 1)
	req = req.WithContext(ctx)
	go func() {
		c <- f(http.DefaultClient.Do(req))
	}()
	select {
	case <-ctx.Done():
		<-c
		return ctx.Err()
	case err := <-c:
		return err
	}
}
