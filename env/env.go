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

// Package env contains the environment that stenographer will set up and run.
// This is the main part of the stenographer server, setting up stenotype's
// environment and running it, and serving all HTTP requests.
package env

import (
	"bytes"
	"fmt"
	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/qxip/stenographer/base"
	"github.com/qxip/stenographer/certs"
	"github.com/qxip/stenographer/config"
	"github.com/qxip/stenographer/filecache"
	"github.com/qxip/stenographer/httputil"
	"github.com/qxip/stenographer/query"
	"github.com/qxip/stenographer/stats"
	"github.com/qxip/stenographer/thread"
	"golang.org/x/net/context"
)

var (
	v               = base.V // verbose logging
	rmHiddenFiles   = stats.S.Get("removed_hidden_files")
	rmMismatchFiles = stats.S.Get("removed_mismatched_files")
)

const (
	fileSyncFrequency = 15 * time.Second

	// These files will be read from Config.CertPath.
	// Use stenokeys.sh to generate them.
	caCertFilename     = "ca_cert.pem"
	serverCertFilename = "server_cert.pem"
	serverKeyFilename  = "server_key.pem"
)

// Serve starts up an HTTP server using http.DefaultServerMux to handle
// requests.  This server will server over TLS, using the certs
// stored in c.CertPath to verify itself to clients and verify clients.
func (e *Env) Serve() error {
	tlsConfig, err := certs.ClientVerifyingTLSConfig(filepath.Join(e.conf.CertPath, caCertFilename))
	if err != nil {
		return fmt.Errorf("cannot verify client cert: %v", err)
	}
	app := fiber.New()
	app.All("/query", e.handleQuery)
	app.All("/debug/stats", stats.S)
	// Create a fasthttp server with Fiber's handler
	server := &fasthttp.Server{
		Handler:   app.Handler(),
		TLSConfig: tlsConfig,
	}

	// Add routes
	//http.HandleFunc("/query", e.handleQuery)
	//http.Handle("/debug/stats", stats.S)
	tlsCertPath := filepath.Join(e.conf.CertPath, serverCertFilename)
	tlsKeyPath := filepath.Join(e.conf.CertPath, serverKeyFilename)
	// Start the Fiber server with TLS
	serverAddr := fmt.Sprintf("%s:%d", e.conf.Host, e.conf.Port)
	if err := server.ListenAndServeTLS(serverAddr, tlsCertPath, tlsKeyPath); err != nil {
		return fmt.Errorf("server failed: %v", err)
	}
	return nil
	//return app.ListenTLS(serverAddr,
	//	filepath.Join(e.conf.CertPath, serverCertFilename),
	//	filepath.Join(e.conf.CertPath, serverKeyFilename))
}

func (e *Env) handleQuery(c *fiber.Ctx) error {
	// Log the request (assuming you have a similar function in your package)
	w := httputil.LogRequest(c, true)
	defer log.Print(w)

	// Parse limit headers
	limit, err := base.LimitFromFiberHeaders(c)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "Invalid Limit Headers")
	}

	// Read the request body
	queryBytes := c.Body()
	q, err := query.NewQuery(string(queryBytes))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "could not parse query")
	}

	// Create a context with a timeout (use fiber.Context's context)
	ctx, cancel := context.WithTimeout(c.Context(), time.Minute*15)
	defer cancel()

	// Lookup packets
	packets := e.Lookup(ctx, q)

	// Set response headers and send the response
	c.Set("Content-Type", "application/octet-stream")
	var buffer bytes.Buffer
	err = base.PacketsToFile(packets, &buffer, limit)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("Failed to write packets: %v", err))
	}
	// Write the buffer content to the response
	return c.Send(buffer.Bytes())
	//if err := base.PacketsToFile(packets, c, limit); err != nil {
	//	return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("Failed to write packets: %v", err))
	//}
}

//func (e *Env) handleQuery(w http.ResponseWriter, r *http.Request) {
//	w = httputil.Log(w, r, true)
//	defer log.Print(w)
//
//	limit, err := base.LimitFromHeaders(r.Header)
//	if err != nil {
//		http.Error(w, "Invalid Limit Headers", http.StatusBadRequest)
//		return
//	}
//
//	queryBytes, err := ioutil.ReadAll(r.Body)
//	if err != nil {
//		http.Error(w, "could not read request body", http.StatusBadRequest)
//		return
//	}
//	q, err := query.NewQuery(string(queryBytes))
//	if err != nil {
//		http.Error(w, "could not parse query", http.StatusBadRequest)
//		return
//	}
//	ctx := httputil.Context(w, r, time.Minute*15)
//	defer ctx.Cancel()
//	packets := e.Lookup(ctx, q)
//	w.Header().Set("Content-Type", "application/octet-stream")
//	base.PacketsToFile(packets, w, limit)
//}

// New returns a new Env for use in running Stenotype.
func New(c config.Config) (_ *Env, returnedErr error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	dirname, err := ioutil.TempDir("", "stenographer")
	if err != nil {
		return nil, fmt.Errorf("couldn't create temp directory: %v", err)
	}
	defer func() {
		// If this fails, remove the temp dir.
		if returnedErr != nil {
			os.RemoveAll(dirname)
		}
	}()
	threads, err := thread.Threads(c.Threads, dirname, filecache.NewCache(c.MaxOpenFiles))
	if err != nil {
		return nil, err
	}
	d := &Env{
		conf:    c,
		name:    dirname,
		threads: threads,
		done:    make(chan bool),
	}
	go d.callEvery(d.syncFiles, fileSyncFrequency)
	return d, nil
}

// args is the set of command line arguments to pass to stentype.
func (d *Env) args() []string {
	res := append(d.conf.Flags,
		fmt.Sprintf("--threads=%d", len(d.conf.Threads)),
		fmt.Sprintf("--dir=%s", d.Path()))

	if len(d.conf.Interface) > 0 {
		res = append(res, fmt.Sprintf("--iface=%s", d.conf.Interface))
	}
	if len(d.conf.TestimonySocket) > 0 {
		res = append(res, fmt.Sprintf("--testimony=%s", d.conf.TestimonySocket))
	}
	return res
}

// stenotype returns a exec.Cmd which runs the stenotype binary with all of
// the appropriate flags.
func (d *Env) stenotype() *exec.Cmd {
	v(0, "Starting stenotype")
	args := d.args()
	v(1, "Starting as %q with args %q", d.conf.StenotypePath, args)
	return exec.Command(d.conf.StenotypePath, args...)
}

// Env contains information necessary to run Stenotype.
type Env struct {
	conf    config.Config
	name    string
	threads []*thread.Thread
	done    chan bool
	fc      *filecache.Cache
	// StenotypeOutput is the writer that stenotype STDOUT/STDERR will be
	// redirected to.
	StenotypeOutput io.Writer
}

// Close closes the directory.  This should only be done when stenotype has
// stopped using it.  After this call, Env should no longer be used.
func (d *Env) Close() error {
	return os.RemoveAll(d.name)
}

func (d *Env) callEvery(cb func(), freq time.Duration) {
	ticker := time.NewTicker(freq)
	defer ticker.Stop()
	cb() // Call function immediately the first time around.
	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			cb()
		}
	}
}

func removeHiddenFilesFrom(dir string) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		log.Printf("Hidden file cleanup failed, could not read directory: %v", err)
		return
	}
	for _, file := range files {
		if file.Mode().IsRegular() && strings.HasPrefix(file.Name(), ".") {
			filename := filepath.Join(dir, file.Name())
			if err := os.Remove(filename); err != nil {
				log.Printf("Unable to remove hidden file %q: %v", filename, err)
			} else {
				rmHiddenFiles.Increment()
				log.Printf("Deleted stale output file %q", filename)
			}
		}
	}
}

func filesIn(dir string) (map[string]os.FileInfo, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]os.FileInfo{}
	for _, file := range files {
		if file.Mode().IsRegular() {
			out[file.Name()] = file
		}
	}
	return out, nil
}

// removeOldFiles removes hidden files from previous runs, as well as packet
// files without indexes and vice versa.
func (d *Env) removeOldFiles() {
	for _, thread := range d.conf.Threads {
		v(1, "Checking %q/%q for stale pkt/idx files...", thread.PacketsDirectory, thread.IndexDirectory)
		removeHiddenFilesFrom(thread.PacketsDirectory)
		removeHiddenFilesFrom(thread.IndexDirectory)
		packetFiles, err := filesIn(thread.PacketsDirectory)
		if err != nil {
			log.Printf("could not get files from %q: %v", thread.PacketsDirectory, err)
			continue
		}
		indexFiles, err := filesIn(thread.IndexDirectory)
		if err != nil {
			log.Printf("could not get files from %q: %v", thread.IndexDirectory, err)
			continue
		}
		var mismatchedFilesToRemove []string
		for file := range packetFiles {
			if indexFiles[file] == nil {
				mismatchedFilesToRemove = append(mismatchedFilesToRemove, filepath.Join(thread.PacketsDirectory, file))
				log.Printf("Removing packet file %q without index found in %q", file, thread.PacketsDirectory)
			}
		}
		for file := range indexFiles {
			if packetFiles[file] == nil {
				mismatchedFilesToRemove = append(mismatchedFilesToRemove, filepath.Join(thread.IndexDirectory, file))
				log.Printf("Removing index file %q without packets found in %q", file, thread.IndexDirectory)
			}
		}
		for _, file := range mismatchedFilesToRemove {
			v(2, "Removing file %q", file)
			if err := os.Remove(file); err != nil {
				log.Printf("Unable to remove mismatched file %q", file)
			} else {
				rmMismatchFiles.Increment()
			}
		}
	}
}

func (d *Env) syncFiles() {
	for _, t := range d.threads {
		t.SyncFiles()
	}
}

// Path returns the underlying directory path for the given Env.
func (d *Env) Path() string {
	return d.name
}

// Lookup looks up the given query in all blockfiles currently known in this
// Env.
func (d *Env) Lookup(ctx context.Context, q query.Query) *base.PacketChan {
	var inputs []*base.PacketChan
	for _, thread := range d.threads {
		inputs = append(inputs, thread.Lookup(ctx, q))
	}
	return base.MergePacketChans(ctx, inputs)
}

// ExportDebugHandlers exports a few debugging handlers to an HTTP ServeMux.
func (d *Env) ExportDebugHandlers(app *fiber.App) {
	app.All("/debug/config", func(c *fiber.Ctx) error {
		//w = httputil.Log(w, r, false)
		logInfo := httputil.LogRequest(c, false)
		defer log.Print(logInfo)
		c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		//	w.Header().Set("Content-Type", "application/json")
		// Respond with the config in JSON format
		return c.JSON(d.conf)
		//json.NewEncoder(w).Encode(d.conf)
	})
	for _, thread := range d.threads {
		thread.ExportDebugHandlers(app)
	}
	oldestTimestamp := stats.S.Get("oldest_timestamp")
	go func() {
		for c := time.Tick(time.Second * 10); ; <-c {
			t := time.Unix(0, 0)
			for _, thread := range d.threads {
				ts := thread.OldestFileTimestamp()
				if ts.After(t) {
					t = ts
				}
			}
			oldestTimestamp.Set(t.UnixNano())
		}
	}()
}

// MinLastFileSeen returns the timestamp of the oldest among the newest files
// created by all threads.
func (d *Env) MinLastFileSeen() time.Time {
	var t time.Time
	for _, thread := range d.threads {
		ls := thread.FileLastSeen()
		if t.IsZero() || ls.Before(t) {
			t = ls
		}
	}
	return t
}

// runStaleFileCheck watches files generated by stenotype to make sure it's
// regularly generating new files.  It will Kill() stenotype if it doesn't see
// at least one new file every maxFileLastSeenDuration in each thread directory.
func (d *Env) runStaleFileCheck(cmd *exec.Cmd, done chan struct{}) {
	ticker := time.NewTicker(maxFileLastSeenDuration)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			v(2, "Checking stenotype for stale files...")
			diff := time.Now().Sub(d.MinLastFileSeen())
			if diff > maxFileLastSeenDuration {
				log.Printf("Restarting stenotype due to stale file.  Age: %v", diff)
				if err := cmd.Process.Kill(); err != nil {
					log.Fatalf("Failed to kill stenotype,  stale file found: %v", err)
				}
			} else {
				v(2, "Stenotype up to date, last file update %v ago", diff)
			}
		case <-done:
			return
		}
	}
}

const (
	minStenotypeRuntimeForRestart = time.Minute
	maxFileLastSeenDuration       = time.Minute * 5
)

// runStenotypeOnce runs the stenotype binary a single time, returning any
// errors associated with its running.
func (d *Env) runStenotypeOnce() error {
	d.removeOldFiles()
	cmd := d.stenotype()
	done := make(chan struct{})
	defer close(done)
	// Start running stenotype.
	cmd.Stdout = d.StenotypeOutput
	cmd.Stderr = d.StenotypeOutput
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start stenotype: %v", err)
	}
	go d.runStaleFileCheck(cmd, done)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("stenotype wait failed: %v", err)
	}
	return fmt.Errorf("stenotype stopped")
}

// RunStenotype keeps the stenotype binary running, restarting it if necessary
// but trying not to allow crash loops.
func (d *Env) RunStenotype() {
	for {
		start := time.Now()
		v(1, "Running Stenotype")
		err := d.runStenotypeOnce()
		duration := time.Since(start)
		log.Printf("Stenotype stopped after %v: %v", duration, err)
		if duration < minStenotypeRuntimeForRestart {
			log.Fatalf("Stenotype ran for too little time, crashing to avoid stenotype crash loop")
		}
	}
}
