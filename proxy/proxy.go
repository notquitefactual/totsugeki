package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

type StriveAPIProxy struct {
	Client         *http.Client
	Server         *http.Server
	GGStriveAPIURL string
	PatchedAPIURL  string
	statsQueue     chan<- *http.Request
	wg             sync.WaitGroup
	cachedNewsReq  *http.Response
	cachedNewsBody []byte
	prediction     StatsGetPrediction
}

type StriveAPIProxyOptions struct {
	AsyncStatsSet   bool
	PredictStatsGet bool
	CacheNews       bool
	NoNews          bool
}

func (s *StriveAPIProxy) proxyRequest(r *http.Request) (*http.Response, error) {
	apiURL, err := url.Parse(s.GGStriveAPIURL) // TODO: Const this
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	apiURL.Path = r.URL.Path

	r.URL = apiURL
	r.Host = ""
	r.RequestURI = ""
	return s.Client.Do(r)
}

// Proxy everything else
func (s *StriveAPIProxy) HandleCatchall(w http.ResponseWriter, r *http.Request) {
	resp, err := s.proxyRequest(r)
	if err != nil {
		fmt.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	// Copy headers
	for name, values := range resp.Header {
		w.Header()[name] = values
	}
	w.WriteHeader(resp.StatusCode)
	reader := io.TeeReader(resp.Body, w) // For dumping API payloads
	payload, err := io.ReadAll(reader)
	// decode the payload as json
	fmt.Println(string(payload))
	if err != nil {
		fmt.Println(err)
	}
}

// print replays to a file
func (s *StriveAPIProxy) HandleReplays(w http.ResponseWriter, r *http.Request) {
	resp, err := s.proxyRequest(r)
	if err != nil {
		fmt.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	// Copy headers
	for name, values := range resp.Header {
		w.Header()[name] = values
	}
	w.WriteHeader(resp.StatusCode)
	reader := io.TeeReader(resp.Body, w) // For dumping API payloads
	payload, err := io.ReadAll(reader)
	if err != nil {
		fmt.Println(err)
	} else {
		f, err := os.OpenFile("replays.txt", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			panic(err)
		}

		defer f.Close()

		if _, err = f.WriteString(string(payload)); err != nil {
			panic(err)
		}

		f2, err := os.OpenFile("replaysRAW.txt", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			fmt.Println("Could not open file")
			panic(err)
		}

		defer f2.Close()

		// we use this as a delimeter when processing the raw data as it is very unlikeyly that we will get a full 8 bytes == 242
		bytesToWrite := append(payload, []byte{242, 242, 242, 242, 242, 242, 242, 242}...)
		if _, err = f2.Write(bytesToWrite); err != nil {
			fmt.Println("Could not write bytes")
			panic(err)
		}
		// decode the payload as json
		fmt.Println(bytesToWrite)
	}
}

// GGST uses the URL from this API after initial launch so we need to intercept this.
func (s *StriveAPIProxy) HandleGetEnv(w http.ResponseWriter, r *http.Request) {
	resp, err := s.proxyRequest(r)
	if err != nil {
		fmt.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	// Copy headers
	for name, values := range resp.Header {
		w.Header()[name] = values
	}
	w.WriteHeader(resp.StatusCode)
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
	}
	buf = bytes.Replace(buf, []byte(s.GGStriveAPIURL), []byte(s.PatchedAPIURL), -1)
	w.Write(buf)
}

// UNSAFE: Cache news on first request. On every other request return the cached value.
func (s *StriveAPIProxy) HandleGetNews(w http.ResponseWriter, r *http.Request) {
	if s.cachedNewsReq != nil {
		for name, values := range s.cachedNewsReq.Header {
			w.Header()[name] = values
		}
		w.Write(s.cachedNewsBody)
	} else {
		resp, err := s.proxyRequest(r)
		if err != nil {
			fmt.Println(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		// Copy headers
		for name, values := range resp.Header {
			w.Header()[name] = values
		}
		w.WriteHeader(resp.StatusCode)
		reader := io.TeeReader(resp.Body, w) // For dumping API payloads
		buf, err := io.ReadAll(reader)
		if err != nil {
			fmt.Println(err)
		}
		s.cachedNewsReq = resp
		s.cachedNewsBody = buf
	}
}

func (s *StriveAPIProxy) Shutdown() {
	fmt.Println("Shutting down proxy...")

	err := s.Server.Shutdown(context.Background())
	if err != nil {
		fmt.Println(err)
	}

	s.stopStatsSender()

	fmt.Println("Waiting for connections to complete...")
	s.wg.Wait()
}

func (s *StriveAPIProxy) ResetStatsGetPrediction() {
	s.prediction.predictionState = reset
}

func CreateStriveProxy(listen string, GGStriveAPIURL string, PatchedAPIURL string, options *StriveAPIProxyOptions) *StriveAPIProxy {
	// Logger
	// make a new logger that logs requests and responses to stdout
	// logger := httplog.NewLogger("TOTSUGEKI", httplog.Options{JSON: false, LogLevel: "trace"})
	transport := http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        2,
		MaxIdleConnsPerHost: 1,
		MaxConnsPerHost:     2,
		IdleConnTimeout:     90 * time.Second, // Drop idle connection after 90 seconds to balance between being nice to ASW and keeping things fast.
	}
	client := http.Client{
		Transport: &transport,
	}

	proxy := &StriveAPIProxy{
		Client:         &client,
		Server:         &http.Server{Addr: listen},
		GGStriveAPIURL: GGStriveAPIURL,
		PatchedAPIURL:  PatchedAPIURL,
	}

	statsSet := proxy.HandleCatchall
	statsGet := proxy.HandleCatchall
	getNews := proxy.HandleCatchall
	r := chi.NewRouter()
	// r.Use(middleware.Logger)
	// r.Use(httplog.RequestLogger(logger))

	if options.AsyncStatsSet {
		statsSet = proxy.HandleStatsSet
		proxy.statsQueue = proxy.startStatsSender()
	}
	if options.PredictStatsGet {
		predictStatsTransport := transport
		predictStatsTransport.MaxIdleConns = StatsGetWorkers
		predictStatsTransport.MaxIdleConnsPerHost = StatsGetWorkers
		predictStatsTransport.MaxConnsPerHost = StatsGetWorkers
		predictStatsTransport.IdleConnTimeout = 10 * time.Second // Quickly drop connections since this is a one-shot.
		predictStatsClient := client
		predictStatsClient.Transport = &predictStatsTransport

		proxy.prediction = CreateStatsGetPrediction(GGStriveAPIURL, &predictStatsClient)
		r.Use(proxy.prediction.StatsGetStateHandler)
		statsGet = func(w http.ResponseWriter, r *http.Request) {
			if !proxy.prediction.HandleGetStats(w, r) {
				proxy.HandleCatchall(w, r)
			}
		}
	}
	if options.NoNews {
		getNews = func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte{})
		}
	} else if options.CacheNews {
		getNews = proxy.HandleGetNews
	}

	r.Route("/api", func(r chi.Router) {
		r.HandleFunc("/sys/get_env", proxy.HandleGetEnv)
		r.HandleFunc("/statistics/get", statsGet)
		r.HandleFunc("/statistics/set", statsSet)
		r.HandleFunc("/catalog/get_replay*", proxy.HandleReplays)
		r.HandleFunc("/sys/get_news", getNews)
		r.HandleFunc("/catalog/get_follow", statsGet)
		r.HandleFunc("/catalog/get_block", statsGet)
		r.HandleFunc("/lobby/get_vip_status", statsGet)
		r.HandleFunc("/item/get_item", statsGet)
		r.HandleFunc("/*", proxy.HandleCatchall)
	})

	proxy.Server.Handler = r
	return proxy
}
