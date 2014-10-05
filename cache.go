//
// Copyright 2014, John Ewart <john@johnewart.net>
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
//

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/rcrowley/go-metrics"
)

type CachedResponse struct {
	Headers    map[string][]string
	Body       []byte
	StatusCode int
}

func memcacheClient() *memcache.Client {
	mc := memcache.New("127.0.0.1:11211")
	return mc
}

func invalidateCache(p *httputil.ReverseProxy) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// Invalidate the cache
		mc := memcacheClient()
		cacheKey := strings.Replace(r.URL.Path, "/", ".", -1)
		cacheKey = strings.TrimPrefix(cacheKey, ".")

		fmt.Printf("Invalidating cache for %s\n", cacheKey)

		if err := mc.Delete(cacheKey); err == memcache.ErrCacheMiss {
			fmt.Printf("No cache needed to clear...\n")
		}

		var err error
		r.URL, err = url.Parse(fmt.Sprintf("http://%s:%d%s?%s", cfg.Chef.ErchefIP, cfg.Chef.ErchefPort, r.URL.Path, r.URL.RawQuery))

		// Do the needful
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			errorHandler(w, fmt.Sprintf("Call to %s failed: %s", r.URL.String(), err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		respBody, err := ioutil.ReadAll(resp.Body)

		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
	}
}

func cacheRequest(p *httputil.ReverseProxy) func(http.ResponseWriter, *http.Request) {

	return func(w http.ResponseWriter, r *http.Request) {
		_, err := dumpBody(r)
		if err != nil {
			errorHandler(w, fmt.Sprintf("Failed to get body from call to %s: %s", r.URL.String(), err), http.StatusBadGateway)
			return
		}
		urlPath := r.URL.Path
		r.URL, err = url.Parse(fmt.Sprintf("http://%s:%d%s?%s", cfg.Chef.ErchefIP, cfg.Chef.ErchefPort, r.URL.Path, r.URL.RawQuery))
		if err != nil {
			errorHandler(w, fmt.Sprintf("Failed to parse URL %s: %s", fmt.Sprintf("http://%s:%d%s?%s", cfg.Chef.ErchefIP, cfg.Chef.ErchefPort, r.URL.Path, r.URL.RawQuery), err), http.StatusBadGateway)
			return
		}

		var response CachedResponse

		metricKey := strings.Replace(urlPath, "/", ".", -1)
		metricKey = strings.TrimPrefix(metricKey, ".")
		//cacheKey := fmt.Sprintf("%s.%s", r.Method, metricKey)
		cacheKey := metricKey
		cacheTimerKey := fmt.Sprintf("%s.%s", metricKey, "cache.query")

		t := metrics.GetOrRegisterTimer(cacheTimerKey, nil)

		mc := memcacheClient()
		var it *memcache.Item

		fmt.Printf("Checking for cached data on %s\n", cacheKey)
		t.Time(func() {
			it, err = mc.Get(cacheKey)
		})

		if err == memcache.ErrCacheMiss {
			fmt.Printf("Cache miss on %s, making call...\n", urlPath)

			timerKey := fmt.Sprintf("%s.%s", metricKey, "time")
			t := metrics.GetOrRegisterTimer(timerKey, nil)

			t.Time(func() {
				// Do the needful
				resp, err := http.DefaultTransport.RoundTrip(r)
				if err != nil {
					errorHandler(w, fmt.Sprintf("Call to %s failed: %s", r.URL.String(), err), http.StatusBadGateway)
					return
				}

				defer resp.Body.Close()

				if err := checkHTTPResponse(resp, []int{http.StatusOK, http.StatusCreated}); err == nil {

				}

				responseBody, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					errorHandler(w, fmt.Sprintf("Failed to get body from call to %s: %s", r.URL.String(), err), http.StatusBadGateway)
					return
				}

				response.Headers = resp.Header
				response.Body = responseBody
				response.StatusCode = resp.StatusCode
				fmt.Printf("Response body: %s\n", responseBody)
			})

			// TODO: don't use JSON (maybe?).
			jsonData, _ := json.Marshal(response)
			fmt.Printf("Cache miss: %s\n", jsonData)

			mc.Set(&memcache.Item{Key: cacheKey, Value: jsonData})
			counterKey := fmt.Sprintf("%s.%s", metricKey, "cache.miss")
			metrics.GetOrRegisterCounter(counterKey, nil).Inc(1)
		} else {
			counterKey := fmt.Sprintf("%s.%s", metricKey, "cache.hit")
			metrics.GetOrRegisterCounter(counterKey, nil).Inc(1)
			json.Unmarshal(it.Value, &response)
			fmt.Printf("Cache hit: %s!\n", it.Value)
		}

		fmt.Println("Rendering response...")
		injectHeaders(w.Header(), response.Headers)
		w.WriteHeader(response.StatusCode)
		w.Write(response.Body)
	}
}

func injectHeaders(dst http.Header, src map[string][]string) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
