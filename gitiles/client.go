// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package gitiles is a client library for the Gitiles source viewer.
package gitiles

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/google/slothfs/cookie"
	"golang.org/x/net/context"
	"golang.org/x/time/rate"
)

// Service is a client for the Gitiles JSON interface.
type Service struct {
	limiter *rate.Limiter
	addr    url.URL
	client  http.Client
	agent   string
	jar     http.CookieJar
	debug   bool
}

// Addr returns the address of the gitiles service.
func (s *Service) Addr() string {
	return s.addr.String()
}

// Options configures the the Gitiles service.
type Options struct {
	// A URL for the Gitiles service.
	Address string

	BurstQPS     int
	SustainedQPS float64

	// Path to a Netscape/Mozilla style cookie file.
	CookieJar string

	// UserAgent defines how we present ourself to the server.
	UserAgent string

	// HTTPClient allows callers to present their own http.Client instead of the default.
	HTTPClient http.Client

	Debug bool
}

var defaultOptions Options

// DefineFlags sets up standard command line flags, and returns the
// options struct in which the values are put.
func DefineFlags() *Options {
	flag.StringVar(&defaultOptions.Address, "gitiles_url", "https://android.googlesource.com", "Set the URL of the Gitiles service.")
	flag.StringVar(&defaultOptions.CookieJar, "gitiles_cookies", "", "Set path to cURL-style cookie jar file.")
	flag.StringVar(&defaultOptions.UserAgent, "gitiles_agent", "slothfs", "Set the User-Agent string to report to Gitiles.")
	flag.Float64Var(&defaultOptions.SustainedQPS, "gitiles_qps", 4, "Set the maximum QPS to send to Gitiles.")
	flag.BoolVar(&defaultOptions.Debug, "gitiles_debug", false, "Print URLs as they are fetched.")
	return &defaultOptions
}

// NewService returns a new Gitiles JSON client.
func NewService(opts Options) (*Service, error) {
	var jar http.CookieJar
	if nm := opts.CookieJar; nm != "" {
		var err error
		jar, err = cookie.NewJar(nm)
		if err != nil {
			return nil, err
		}
		if err := cookie.WatchJar(jar, nm); err != nil {
			return nil, err
		}
	}

	if opts.SustainedQPS == 0.0 {
		opts.SustainedQPS = 4
	}
	if opts.BurstQPS == 0 {
		opts.BurstQPS = int(10.0 * opts.SustainedQPS)
	} else if float64(opts.BurstQPS) < opts.SustainedQPS {
		opts.BurstQPS = int(opts.SustainedQPS) + 1
	}

	url, err := url.Parse(opts.Address)
	if err != nil {
		return nil, err
	}
	s := &Service{
		limiter: rate.NewLimiter(rate.Limit(opts.SustainedQPS), opts.BurstQPS),
		addr:    *url,
		agent:   opts.UserAgent,
		client:  opts.HTTPClient,
	}

	s.client.Jar = jar
	s.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		req.Header.Set("User-Agent", s.agent)
		return nil
	}
	s.debug = opts.Debug
	return s, nil
}

func (s *Service) get(u *url.URL) ([]byte, error) {
	ctx := context.Background()

	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("User-Agent", s.agent)
	resp, err := s.client.Do(req)

	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", u.String(), resp.Status)
	}

	if s.debug {
		log.Printf("%s %s: %d", req.Method, req.URL, resp.StatusCode)
	}
	if got := resp.Request.URL.String(); got != u.String() {
		// We accept redirects, but only for authentication.
		// If we get a 200 from a different page than we
		// requested, it's probably some sort of login page.
		return nil, fmt.Errorf("got URL %s, want %s", got, u.String())
	}

	c, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.Header.Get("Content-Type") == "text/plain; charset=UTF-8" {
		out := make([]byte, base64.StdEncoding.DecodedLen(len(c)))
		n, err := base64.StdEncoding.Decode(out, c)
		return out[:n], err
	}
	return c, nil
}

var xssTag = []byte(")]}'\n")

func (s *Service) getJSON(u *url.URL, dest interface{}) error {
	c, err := s.get(u)
	if err != nil {
		return err
	}

	if !bytes.HasPrefix(c, xssTag) {
		return fmt.Errorf("Gitiles JSON %s missing XSS tag: %q", u, c)
	}
	c = c[len(xssTag):]

	err = json.Unmarshal(c, dest)
	if err != nil {
		err = fmt.Errorf("Unmarshal(%s): %v", u, err)
	}
	return err
}

// List retrieves the list of projects.
func (s *Service) List(branches []string) (map[string]*Project, error) {
	listURL := s.addr
	listURL.RawQuery = "format=JSON"
	for _, b := range branches {
		listURL.RawQuery += "&b=" + b
	}

	projects := map[string]*Project{}
	err := s.getJSON(&listURL, &projects)
	for k, v := range projects {
		if k != v.Name {
			return nil, fmt.Errorf("gitiles: key %q had project name %q", k, v.Name)
		}
	}

	return projects, err
}

// NewRepoService creates a service for a specific repository on a Gitiles server.
func (s *Service) NewRepoService(name string) *RepoService {
	return &RepoService{
		Name:    name,
		service: s,
	}
}

// RepoService is a JSON client for the functionality of a specific
// respository.
type RepoService struct {
	Name    string
	service *Service
}

// Get retrieves a single project.
func (s *RepoService) Get() (*Project, error) {
	jsonURL := s.service.addr
	jsonURL.Path = path.Join(jsonURL.Path, s.Name)
	jsonURL.RawQuery = "format=JSON"

	var p Project
	err := s.service.getJSON(&jsonURL, &p)
	return &p, err
}

// GetBlob fetches a blob.
func (s *RepoService) GetBlob(branch, filename string) ([]byte, error) {
	blobURL := s.service.addr

	blobURL.Path = path.Join(blobURL.Path, s.Name, "+show", branch, filename)
	blobURL.RawQuery = "format=TEXT"

	// TODO(hanwen): invent a more structured mechanism for logging.
	log.Println(blobURL.String())
	return s.service.get(&blobURL)
}

// GetTree fetches a tree. The dir argument may not point to a
// blob. If recursive is given, the server recursively expands the
// tree.
func (s *RepoService) GetTree(branch, dir string, recursive bool) (*Tree, error) {
	jsonURL := s.service.addr
	jsonURL.Path = path.Join(jsonURL.Path, s.Name, "+", branch, dir)
	if !strings.HasSuffix(jsonURL.Path, "/") {
		jsonURL.Path += "/"
	}
	jsonURL.RawQuery = "format=JSON&long=1"

	if recursive {
		jsonURL.RawQuery += "&recursive=1"
	}

	var tree Tree
	err := s.service.getJSON(&jsonURL, &tree)
	return &tree, err
}

// GetCommit gets the data of a commit in a branch.
func (s *RepoService) GetCommit(branch string) (*Commit, error) {
	jsonURL := s.service.addr
	jsonURL.Path = path.Join(jsonURL.Path, s.Name, "+", branch)
	jsonURL.RawQuery = "format=JSON"

	var c Commit
	err := s.service.getJSON(&jsonURL, &c)
	return &c, err
}
