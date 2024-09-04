// Copyright 2014 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package httputil provides http utilities for stenographer.
package httputil

import (
	"bytes"
	"fmt"
	"github.com/gofiber/fiber/v2"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/qxip/stenographer/base"
	"github.com/qxip/stenographer/stats"
)

// Context returns a new context.Content that cancels when the
// underlying http.ResponseWriter closes.
func Context(w http.ResponseWriter, r *http.Request, timeout time.Duration) base.Context {
	ctx := base.NewContext(timeout)
	if closer, ok := w.(http.CloseNotifier); ok {
		go func() {
			select {
			case <-closer.CloseNotify():
				log.Printf("Detected closed HTTP connection, canceling query")
				ctx.Cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx
}

type LogStruct struct {
	start      time.Time
	body       string
	err        error
	statusCode int
}

type httpLog struct {
	r      *http.Request
	w      http.ResponseWriter
	nBytes int
	code   int
	err    error
	start  time.Time
	body   string
}

// New returns a new ResponseWriter which provides a nice
// String() method for easy printing.  The expected usage is:
//
//	func (h *myHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
//	  w = httputil.Log(w, r, false)
//	  defer log.Print(w)  // Prints out useful information about request AND response
//	  ... do stuff ...
//	}
func Log(w http.ResponseWriter, r *http.Request, logRequestBody bool) http.ResponseWriter {
	h := &httpLog{w: w, r: r, start: time.Now(), code: http.StatusOK}
	if logRequestBody {
		var buf bytes.Buffer
		_, h.err = io.Copy(&buf, r.Body)
		r.Body.Close()
		r.Body = ioutil.NopCloser(&buf)
		h.body = fmt.Sprintf(" RequestBody:%q", buf.String())
	}
	return h
}

// Function to handle logging within a route
func LogRequest(c *fiber.Ctx, logRequestBody bool) *LogStruct {
	logInfo := &LogStruct{
		start:      time.Now(),
		statusCode: fiber.StatusOK,
	}

	// Log request body if required
	if logRequestBody {
		var buf bytes.Buffer
		// Read the body into the buffer
		_, logInfo.err = io.Copy(&buf, c.Request().BodyStream())
		// Reset the request body stream with the buffered content
		c.Request().SetBody(buf.Bytes())
		// Store the body content for logging
		logInfo.body = fmt.Sprintf(" RequestBody:%q", buf.String())
	}

	// Log request information
	log.Printf("Request started at: %v, Method: %s, URL: %s%s",
		logInfo.start, c.Method(), c.OriginalURL(), logInfo.body)

	return logInfo
}

// Header implements http.ResponseWriter.
func (h *httpLog) Header() http.Header {
	return h.w.Header()
}

// Write implements http.ResponseWriter and io.Writer.
func (h *httpLog) Write(data []byte) (int, error) {
	n, err := h.w.Write(data)
	h.nBytes += n
	if err != nil && h.err == nil {
		h.err = err
	}
	return n, err
}

// WriteHeader implements http.ResponseWriter.
func (h *httpLog) WriteHeader(code int) {
	h.code = code
	h.w.WriteHeader(code)
}

// String implements fmt.Stringer.
func (h *httpLog) String() string {
	var errstr string
	if h.err != nil {
		errstr = h.err.Error()
	}
	duration := time.Since(h.start)
	prefix := "http_request" + strings.Replace(h.r.URL.Path, "/", "_", -1) + "_" + h.r.Method + "_"
	stats.S.Get(prefix + "completed").Increment()
	stats.S.Get(prefix + "nanos").IncrementBy(duration.Nanoseconds())
	stats.S.Get(prefix + "bytes").IncrementBy(int64(h.nBytes))
	return fmt.Sprintf("Requester:%q Request:\"%v %v %v\" Time:%v Bytes:%v Code:%q Err:%q%v",
		h.r.RemoteAddr,
		h.r.Method,
		h.r.URL,
		h.r.Proto,
		duration,
		h.nBytes,
		http.StatusText(h.code),
		errstr,
		h.body)
}
