package main

import (
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/itchyny/gojq"
	"github.com/pschou/go-params"
	"github.com/vishvananda/netns"
)

var (
	version = "debug"
	debug   = false
	Headers = map[string]string{
		"content-type": "application/json",
	}

	JQString string
	keypair  tls.Certificate

	raw, includeHeader, certIgnore, flush, useCache, followRedirects, pretty bool
	cert, key, ca, cacheDir, method, postData, outputFile                    string
	maxTries                                                                 int
	delay, maxAge, timeout                                                   time.Duration
	headerVals                                                               *headerValue
	caCertPool                                                               *x509.CertPool

	dat        map[string]interface{}
	Args       []string
	urls       [](*url.URL)
	cacheFiles []string

	docker string
)

type headerValue string

func (h *headerValue) Set(val []string) error {
	parts := strings.SplitN(val[0], ":", 2)
	if len(parts) < 2 {
		return errors.New("Malformatted header")
	}
	Headers[strings.ToLower(strings.TrimSpace(parts[0]))] = strings.TrimPrefix(parts[1], " ")
	return nil
}
func (h *headerValue) Get() interface{} { return "" }
func (h *headerValue) String() string   { return "\"content-type: application/json\"" }

func main() {
	params.Default = "Default="
	params.PresVar(&pretty, "pretty P", "Pretty print JSON with indents")
	params.PresVar(&flush, "flush", "Force redownload, when using cache")
	params.PresVar(&useCache, "cache C", "Use local cache to speed up static queries")
	params.PresVar(&debug, "debug", "Debug / verbose output")
	params.PresVar(&raw, "raw-output r", "Raw output, no quotes for strings")
	params.PresVar(&includeHeader, "include i", "Include header in output")
	temp := os.Getenv("TEMP")
	if len(temp) > 4 && temp[1:2] == ":\\" {
		// use windows temp directory name
	} else {
		temp = os.TempDir()
	}
	params.StringVar(&cacheDir, "cachedir", temp, "Path for cache", "DIR")
	params.StringVar(&outputFile, "output o", "", "Write output to <file> instead of stdout", "FILE")
	params.DurationVar(&maxAge, "max-age", 4*time.Hour, "Max age for cache", "DURATION")
	params.GroupingSet("Request")
	params.StringVar(&postData, "data d", "", "Data to use in POST (use @filename to read from file)", "STRING")
	params.Var(headerVals, "header H", "Custom header to pass to server\n", "'HEADER: VALUE'", 1)
	params.PresVar(&followRedirects, "location L", "Follow redirects")
	params.DurationVar(&delay, "retry-delay", 7*time.Second, "Delay between retries", "DURATION")
	params.DurationVar(&timeout, "max-time m", 15*time.Second, "Timeout per request", "DURATION")
	params.IntVar(&maxTries, "max-tries", 30, "Maximum number of tries", "TRIES")
	params.PresVar(&certIgnore, "insecure k", "Ignore certificate validation checks")
	params.StringVar(&method, "request X", "GET", "Method to use for HTTP request (ie: POST/GET)", "METHOD")
	params.StringVar(&docker, "docker", "", "Switch to the network of a container", "CONTAINER_ID")

	params.Usage = func() {
		fmt.Println("jqURL - URL and JSON parser tool, Written by Paul Schou (github.com/pschou/jqURL), Version: " + version)
		fmt.Printf("Usage:\n  %s [options] \"JSON Parser\" URLs\n\n", os.Args[0])
		params.PrintDefaults()
	}

	params.GroupingSet("Certificate")
	params.StringVar(&ca, "cacert", "", "Use certificate authorities, PEM encoded", "FILE")
	params.StringVar(&cert, "cert E", "", "Use client cert in request, PEM encoded", "FILE")
	params.StringVar(&key, "key", "", "Key file for client cert, PEM encoded", "FILE")

	params.CommandLine.Indent = 2
	params.Parse()
	Args = params.Args()

	if ca != "" {
		caCert, err := ioutil.ReadFile(ca)
		if err != nil {
			log.Fatalf("Error reading CA cert file %q: %s", ca, err)
		}
		caCertPool = x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
	}

	if cert != "" && key == "" {
		// Just in case the cert and key are in the same file
		key = cert
	}
	if cert != "" && key != "" {
		var err error
		keypair, err = tls.LoadX509KeyPair(cert, key)
		if err != nil {
			log.Fatalf("Error reading client cert keypair cert=%q key=%q: %s", cert, key, err)
		}
	}

	if len(Args) < 2 {
		params.Usage()
		os.Exit(1)
		return
	}

	JQString = Args[0]
	Args = Args[1:]
	cacheFiles = make([]string, len(Args))
	urls = make([](*url.URL), len(Args))

	for i, Arg := range Args {
		u, err := url.Parse(Arg)
		if err != nil {
			fmt.Println("Malformed URL:", err)
			os.Exit(1)
		}
		urls[i] = u
	}

	for i, Arg := range Args {
		h := sha1.New()
		h.Write([]byte(Arg))
		h.Write([]byte(fmt.Sprintf("%d", os.Getuid())))
		bs := h.Sum(nil)

		cacheFile := fmt.Sprintf("%s/jqurl_%x", cacheDir, bs)
		cacheFiles[i] = cacheFile

		stat, err := os.Stat(cacheFile)
		if err == nil && !flush && useCache && time.Now().Add(maxAge).After(stat.ModTime()) {
			if debug {
				log.Println("found cache", cacheFile)
			}
			byt, err := ioutil.ReadFile(cacheFile)
			if err == nil {
				if debug {
					log.Println("using cache", cacheFile)
				}
				if includeHeader {
					fmt.Fprintf(os.Stderr, "Header skipped as cache used\nURL: %s\nFile: %s\n", urls[i], cacheFile)
				}
				json.Unmarshal(byt, &dat)
				break
			}
		}
	}

	if docker != "" {
		// Lock the OS Thread so we don't accidentally switch namespaces
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// Save the current network namespace
		origns, _ := netns.Get()
		defer origns.Close()

		nsh, err := netns.GetFromDocker(docker)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error switching to container network space %q, err: %s", docker, err)
			os.Exit(1)
		}
		defer nsh.Close()
		netns.Set(nsh)
	}

	doCurl()
}

func doCurl() {
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{
		InsecureSkipVerify: certIgnore,
		RootCAs:            caCertPool,
		Certificates:       []tls.Certificate{keypair},
		Renegotiation:      tls.RenegotiateOnceAsClient,
	}
	//http.DefaultTransport.IdleConnTimeout = 10 * time.Second
	client := &http.Client{
		Transport: http.DefaultTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if followRedirects == false {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	for j := 0; j < maxTries && len(dat) == 0; j++ {
		i := j % len(Args)
		if debug {
			log.Println("HTTP", method, urls[i])
		}
		var err error
		var resp *http.Response
		var req *http.Request

		var rdr io.Reader
		if method == "POST" {
			if len(postData) > 0 && postData[0] == '@' {
				f, err := os.Open(postData[1:])
				if err != nil {
					log.Fatalf("Unable to open %q, err: %s", postData[1:], err)
				}
				defer f.Close()
				rdr = f
			} else {
				rdr = strings.NewReader(postData)
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		req, err = http.NewRequestWithContext(ctx, method, urls[i].String(), rdr)
		if err != nil {
			log.Fatalf("New request error: %s", err)
		}
		if method == "POST" {
			req.Header.Set("Content-Type", "x-www-form-urlencoded")
		}
		for key, val := range Headers {
			if debug {
				fmt.Printf("Request Header: %s: %s\n", key, val)
			}
			req.Header.Set(key, val)
		}
		resp, err = client.Do(req)
		if debug && err != nil {
			fmt.Printf("Error doing http request: %s\n", err)
		}

		if err == nil {
			if includeHeader {
				fmt.Fprintf(os.Stderr, "%s %s\n", resp.Proto, resp.Status)
				for key, vals := range resp.Header {
					for _, val := range vals {
						fmt.Fprintf(os.Stderr, "%s: %s\n", key, val)
					}
				}
				fmt.Fprintf(os.Stderr, "\n")
			}

			byt, err := ioutil.ReadAll(resp.Body)
			resp.Body.Close()

			if err == nil {
				err = json.Unmarshal(byt, &dat)
				if err != nil && debug {
					log.Fatalf("Cannot unmarshall url %q err: %s", urls[i], err)
				}
				if err == nil {
					if !useCache {
						break
					}
					if debug {
						log.Println("writing out file")
					}
					err = ioutil.WriteFile(cacheFiles[i], byt, 0666)
					if err != nil && debug {
						log.Fatalf("Error writing file: %s", err)
					}
					break
				}
			}
		}

		if i%len(Args) == len(Args)-1 {
			time.Sleep(delay)
		}
	}

	query, err := gojq.Parse(JQString)
	if err != nil {
		log.Fatalf("Error compiling jq query %q: %s", JQString, err)
	}
	iter := query.Run(dat) // or query.RunWithContext
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			log.Fatalf("Error running jq query %q: %s", JQString, err)
		}
		if debug {
			fmt.Printf("%#v\n", v)
		}

		output := os.Stdout
		if outputFile != "" {
			f, err := os.Create(outputFile)
			if err != nil {
				log.Fatalf("Error creating output file: %s", err)
			}
			defer f.Close()
			output = f
		}

		if raw {
			fmt.Fprintf(output, "%v\n", v)
		} else {
			var jsonOutput []byte
			if pretty {
				jsonOutput, _ = json.MarshalIndent(v, "", "  ")
			} else {
				jsonOutput, _ = json.Marshal(v)
			}
			fmt.Fprintf(output, "%s\n", string(jsonOutput))
		}
	}
}
