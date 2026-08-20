// Package pdftext extracts bounded plain text from local PDF files.
package pdftext

import (
	"fmt"
	"os"
	"strings"

	"github.com/ledongthuc/pdf"
)

const (
	DefaultMaxFileSize  int64 = 20 << 20
	DefaultMaxPages           = 500
	DefaultMaxTextBytes       = 512 * 1024
)

// Options bounds file size, pages, and accumulated extracted text.
type Options struct {
	MaxFileSize            int64
	MaxPages, MaxTextBytes int
}

// DefaultOptions returns Mora's production PDF extraction bounds.
func DefaultOptions() Options {
	return Options{MaxFileSize: DefaultMaxFileSize, MaxPages: DefaultMaxPages, MaxTextBytes: DefaultMaxTextBytes}
}

// Extract returns bounded plain text and converts parser panics into errors.
func Extract(path string, opts Options) (text string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			text, err = "", fmt.Errorf("pdf parse panicked (malformed input): %v", recovered)
		}
	}()
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > opts.MaxFileSize {
		return "", fmt.Errorf("pdf too large: %d bytes (cap %d)", info.Size(), opts.MaxFileSize)
	}
	file, reader, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var out strings.Builder
	pages := reader.NumPage()
	if pages > opts.MaxPages {
		pages = opts.MaxPages
	}
	for pageNumber := 1; pageNumber <= pages; pageNumber++ {
		page := reader.Page(pageNumber)
		if page.V.IsNull() {
			continue
		}
		fonts := make(map[string]*pdf.Font)
		for _, name := range page.Fonts() {
			font := page.Font(name)
			fonts[name] = &font
		}
		body, pageErr := page.GetPlainText(fonts)
		if pageErr != nil {
			continue
		}
		out.WriteString(body)
		out.WriteString("\n")
		if out.Len() > opts.MaxTextBytes {
			break
		}
	}
	return strings.TrimSpace(out.String()), nil
}
