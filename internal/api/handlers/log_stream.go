package handlers

import (
	"bufio"
	"errors"
	"io"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

const (
	defaultLogBufferSize = 64 * 1024
	maxLogLineSize       = 1024 * 1024
)

func newLogStreamScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, defaultLogBufferSize), maxLogLineSize)
	return scanner
}

func logStreamErrorMessage(err error) string {
	if errors.Is(err, bufio.ErrTooLong) {
		return "log line too long, stream interrupted"
	}
	return "log stream interrupted"
}

// followLogStreamSSE streams log lines as SSE messages, writing a comment
// line every 30s so proxies do not cut the stream while the pod is quiet,
// exactly like the watch endpoints. The scanner runs in its own goroutine
// because it blocks on the pod's log stream; the caller's deferred
// stream.Close() unblocks it when the handler returns.
func followLogStreamSSE(c *gin.Context, stream io.Reader) {
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Flush()

	lines := make(chan string)
	scanErr := make(chan error, 1)
	go func() {
		scanner := newLogStreamScanner(stream)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-c.Request.Context().Done():
				close(lines)
				return
			}
		}
		scanErr <- scanner.Err()
		close(lines)
	}()

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-keepalive.C:
			// A comment line keeps proxies from cutting an idle stream.
			if _, err := w.WriteString(": keepalive\n\n"); err != nil {
				return
			}
			w.Flush()
		case line, ok := <-lines:
			if !ok {
				// scanErr is filled before lines is closed on the scanner
				// path; it stays empty when the client went away first.
				select {
				case err := <-scanErr:
					if err != nil {
						logrus.WithError(err).Warn("Log stream ended on a scanner error")
						c.SSEvent("error", logStreamErrorMessage(err))
						w.Flush()
					}
				default:
				}
				return
			}
			c.SSEvent("message", line)
			w.Flush()
		}
	}
}
