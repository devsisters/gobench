package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
)

var intrrupted int32 = 0

var (
	requests         int64
	period           int64
	clients          int
	url              string
	urlsFilePath     string
	keepAlive        bool
	postDataFilePath string
	connectTimeout   int
	writeTimeout     int
	readTimeout      int
	requestTimeout   int
)

type Configuration struct {
	urls      []string
	method    string
	postData  []byte
	requests  int64
	period    int64
	keepAlive bool
}

type Result struct {
	requests             int64
	success              int64
	networkFailed        int64
	networkReadFailed    int64
	networkWriteFailed   int64
	requestTimeoutFailed int64
	badFailed            int64
}

type MyConn struct {
	net.Conn
	m_readTimeout  time.Duration
	m_writeTimeout time.Duration
	m_result       *Result
}

func (this *MyConn) Read(b []byte) (n int, err error) {
	len, err := this.Conn.Read(b)

	this.Conn.SetReadDeadline(time.Now().Add(this.m_readTimeout))

	if err != nil {
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
			this.m_result.networkReadFailed++
			return len, nil
		}

		this.m_result.networkFailed++
	}

	return len, nil
}

func (this *MyConn) Write(b []byte) (n int, err error) {
	len, err := this.Conn.Write(b)

	this.Conn.SetWriteDeadline(time.Now().Add(this.m_writeTimeout))

	if err != nil {
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
			this.m_result.networkWriteFailed++
			return len, nil
		}

		this.m_result.networkFailed++
	}

	return len, nil
}

func init() {
	flag.Int64Var(&requests, "r", -1, "Number of requests per client")
	flag.IntVar(&clients, "c", 100, "Number of concurrent clients")
	flag.StringVar(&url, "u", "", "URL")
	flag.StringVar(&urlsFilePath, "f", "", "URL's file path (line seperated)")
	flag.BoolVar(&keepAlive, "k", true, "Do HTTP keep-alive")
	flag.StringVar(&postDataFilePath, "d", "", "HTTP POST data file path")
	flag.Int64Var(&period, "t", -1, "Period of time (in seconds)")
	flag.IntVar(&connectTimeout, "tc", 5000, "Connect timeout (in milliseconds)")
	flag.IntVar(&writeTimeout, "tw", 5000, "Write timeout (in milliseconds)")
	flag.IntVar(&readTimeout, "tr", 5000, "Read timeout (in milliseconds)")
	flag.IntVar(&requestTimeout, "ta", 5000, "Request timeout (in milliseconds)")
}

func printResults(c chan *Result, startTime time.Time) {
	var requests int64
	var success int64
	var networkFailed int64
	var networkReadFailed int64
	var networkWriteFailed int64
	var badFailed int64
	var requestTimeoutFailed int64

	for i := 0; i < clients; i++ {
		result := <-c
		requests += result.requests
		success += result.success
		networkFailed += result.networkFailed
		networkReadFailed += result.networkReadFailed
		networkWriteFailed += result.networkWriteFailed
		badFailed += result.badFailed
		requestTimeoutFailed += result.requestTimeoutFailed
	}

	elapsed := int64(time.Since(startTime).Seconds())

	if elapsed == 0 {
		elapsed = 1
	}

	fmt.Println()
	fmt.Printf("Requests:                       %10d hits\n", requests)
	fmt.Printf("Successful requests:            %10d hits\n", success)
	fmt.Printf("Network failed:                 %10d hits\n", networkFailed)
	fmt.Printf("Network reads failed:           %10d reads\n", networkReadFailed)
	fmt.Printf("Network writes failed:          %10d writes\n", networkWriteFailed)
	fmt.Printf("Requests timeout failed:        %10d hits\n", requestTimeoutFailed)
	fmt.Printf("Bad requests failed (!2xx):     %10d hits\n", badFailed)
	fmt.Printf("Requests rate:                  %10d hits/sec\n", success/elapsed)
	fmt.Printf("Test time:                      %10d sec\n", elapsed)
}

func readLines(path string) (lines []string, err error) {

	var file *os.File
	var part []byte
	var prefix bool

	if file, err = os.Open(path); err != nil {
		return
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	buffer := bytes.NewBuffer(make([]byte, 0))
	for {
		if part, prefix, err = reader.ReadLine(); err != nil {
			break
		}
		buffer.Write(part)
		if !prefix {
			lines = append(lines, buffer.String())
			buffer.Reset()
		}
	}
	if err == io.EOF {
		err = nil
	}
	return
}

func interrupted() bool {
	return atomic.LoadInt32(&intrrupted) != 0
}

func interrupt() {
	atomic.StoreInt32(&intrrupted, 1)
}

func NewConfiguration() *Configuration {

	if urlsFilePath == "" && url == "" {
		flag.Usage()
		os.Exit(1)
	}

	if requests == -1 && period == -1 {
		fmt.Println("Requests or period must be provided")
		flag.Usage()
		os.Exit(1)
	}

	if requests != -1 && period != -1 {
		fmt.Println("Only one should be provided: [requests|period]")
		flag.Usage()
		os.Exit(1)
	}

	configuration := &Configuration{
		urls:      make([]string, 0),
		method:    "GET",
		postData:  nil,
		keepAlive: keepAlive,
		requests:  int64((1 << 63) - 1)}

	if period != -1 {
		configuration.period = period

		timeout := make(chan bool, 1)
		go func() {
			<-time.After(time.Duration(period) * time.Second)
			timeout <- true
		}()

		go func() {
			<-timeout
			interrupt()
		}()
	}

	if requests != -1 {
		configuration.requests = requests
	}

	if urlsFilePath != "" {
		fileLines, err := readLines(urlsFilePath)

		if err != nil {
			log.Fatalf("Error in ioutil.ReadFile for file: %s Error: ", urlsFilePath, err)
		}

		configuration.urls = fileLines
	}

	if url != "" {
		configuration.urls = append(configuration.urls, url)
	}

	if postDataFilePath != "" {
		configuration.method = "POST"

		data, err := ioutil.ReadFile(postDataFilePath)

		if err != nil {
			log.Fatalf("Error in ioutil.ReadFile for file path: %s Error: ", postDataFilePath, err)
		}

		configuration.postData = data
	}

	return configuration
}

func TimeoutDialer(result *Result, connectTimeout, readTimeout, writeTimeout time.Duration) func(net, address string) (conn net.Conn, err error) {
	return func(mynet, address string) (net.Conn, error) {
		conn, err := net.DialTimeout(mynet, address, connectTimeout)
		if err != nil {
			return nil, err
		}

		conn.SetReadDeadline(time.Now().Add(readTimeout))
		conn.SetWriteDeadline(time.Now().Add(writeTimeout))

		myConn := &MyConn{Conn: conn, m_readTimeout: readTimeout, m_writeTimeout: writeTimeout, m_result: result}

		return myConn, nil
	}
}

func MyClient(result *Result, connectTimeout, readTimeout, writeTimeout time.Duration) *http.Client {

	return &http.Client{
		Transport: &http.Transport{
			Dial:            TimeoutDialer(result, connectTimeout, readTimeout, writeTimeout),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

func client(configuration *Configuration, c chan *Result) {

	result := &Result{}

	myclient := MyClient(result, time.Duration(connectTimeout)*time.Millisecond,
		time.Duration(readTimeout)*time.Millisecond,
		time.Duration(writeTimeout)*time.Millisecond)

	for result.requests < configuration.requests && !interrupted() {
		for _, tmpUrl := range configuration.urls {
			before := time.Now()
			req, _ := http.NewRequest(configuration.method, tmpUrl, bytes.NewReader(configuration.postData))

			if configuration.keepAlive == true {
				req.Header.Add("Connection", "keep-alive")
			} else {
				req.Header.Add("Connection", "close")
			}

			resp, err := myclient.Do(req)
			result.requests++

			if err != nil {
				result.networkFailed++
				continue
			}

			_, errRead := ioutil.ReadAll(resp.Body)

			if errRead != nil {
				continue
			}

			if !time.Now().Before(before.Add(time.Duration(requestTimeout) * time.Millisecond)) {
				result.requestTimeoutFailed++
				continue
			}

			if resp.StatusCode == http.StatusOK {
				result.success++
			} else {
				result.badFailed++
			}

			resp.Body.Close()
		}
	}

	c <- result
}

func main() {

	signalChannel := make(chan os.Signal, 2)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM)
	go func() {
		_ = <-signalChannel
		interrupt()
	}()

	flag.Parse()

	configuration := NewConfiguration()

	goMaxProcs := os.Getenv("GOMAXPROCS")

	if goMaxProcs == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	resultChannel := make(chan *Result)

	fmt.Printf("Dispatching %d clients\n", clients)

	startTime := time.Now()
	for i := 0; i < clients; i++ {

		go client(configuration, resultChannel)

	}
	fmt.Println("Waiting for results...")
	printResults(resultChannel, startTime)
}
