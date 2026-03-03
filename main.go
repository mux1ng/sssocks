package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	proxy1 "golang.org/x/net/proxy"
)

// 测试代理是否可用
func testProxy(proxy string, wg *sync.WaitGroup, results chan<- string) {
	defer wg.Done()

	parts := strings.Split(proxy, ":")
	if len(parts) != 2 {
		return
	}

	host := parts[0]
	port := parts[1]

	baseDialer := &net.Dialer{
		Timeout: 5 * time.Second,
	}

	dialer, err := proxy1.SOCKS5("tcp", fmt.Sprintf("%s:%s", host, port), nil, baseDialer)
	if err != nil {
		return
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	resp, err := client.Get("http://ifconfig.me")
	if err == nil && resp.StatusCode == 200 {
		results <- proxy
		resp.Body.Close()
	}
}

// 启动本地 SOCKS5 代理服务器
func startProxyServer(db *sql.DB) {
	listener, err := net.Listen("tcp", ":8088")
	if err != nil {
		log.Fatalf("Failed to start proxy server: %v", err)
	}
	log.Println("Starting proxy server on :8088")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down server...")
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				break
			}
			log.Printf("Failed to accept connection: %v", err)
			continue
		}
		go handleConnection(conn, db)
	}
}

// 处理代理连接
func handleConnection(conn net.Conn, db *sql.DB) {
	defer conn.Close()

	maxRetries := 5
	for i := 0; i < maxRetries; i++ {
		row := db.QueryRow("SELECT proxy FROM proxies ORDER BY RANDOM() LIMIT 1")
		var proxy string
		err := row.Scan(&proxy)
		if err != nil {
			log.Printf("No available proxy: %v", err)
			return
		}

		parts := strings.Split(proxy, ":")
		if len(parts) != 2 {
			continue
		}

		host := parts[0]
		port := parts[1]

		dialer, err := proxy1.SOCKS5("tcp", fmt.Sprintf("%s:%s", host, port), nil, proxy1.Direct)
		if err != nil {
			log.Printf("Failed to connect to SOCKS5 proxy %s:%s: %v, retrying...", host, port, err)
			continue
		}

		if err := socks5Handshake(conn, dialer); err != nil {
			log.Printf("SOCKS5 handshake failed: %v, retrying...", err)
			continue
		}
		return
	}
	log.Printf("All proxy retries failed")
}

func socks5Handshake(clientConn net.Conn, dialer proxy1.Dialer) error {
	buf := make([]byte, 256)

	_, err := clientConn.Read(buf[:2])
	if err != nil {
		return fmt.Errorf("read version and nmethods: %v", err)
	}

	version := buf[0]
	nmethods := buf[1]

	if version != 5 {
		return fmt.Errorf("unsupported SOCKS version: %d", version)
	}

	_, err = clientConn.Read(buf[:nmethods])
	if err != nil {
		return fmt.Errorf("read methods: %v", err)
	}

	_, err = clientConn.Write([]byte{5, 0})
	if err != nil {
		return fmt.Errorf("write method selection: %v", err)
	}

	_, err = clientConn.Read(buf[:4])
	if err != nil {
		return fmt.Errorf("read request header: %v", err)
	}

	cmd := buf[1]
	atyp := buf[3]

	if cmd != 1 {
		clientConn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return fmt.Errorf("unsupported command: %d", cmd)
	}

	var targetAddr string
	var targetPort uint16

	switch atyp {
	case 1:
		_, err = clientConn.Read(buf[:6])
		if err != nil {
			return fmt.Errorf("read IPv4 address: %v", err)
		}
		targetAddr = fmt.Sprintf("%d.%d.%d.%d", buf[0], buf[1], buf[2], buf[3])
		targetPort = uint16(buf[4])<<8 | uint16(buf[5])
	case 3:
		_, err = clientConn.Read(buf[:1])
		if err != nil {
			return fmt.Errorf("read domain length: %v", err)
		}
		domainLen := buf[0]
		_, err = clientConn.Read(buf[:domainLen+2])
		if err != nil {
			return fmt.Errorf("read domain and port: %v", err)
		}
		targetAddr = string(buf[:domainLen])
		targetPort = uint16(buf[domainLen])<<8 | uint16(buf[domainLen+1])
	case 4:
		_, err = clientConn.Read(buf[:18])
		if err != nil {
			return fmt.Errorf("read IPv6 address: %v", err)
		}
		targetAddr = fmt.Sprintf("[%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x]",
			buf[0], buf[1], buf[2], buf[3], buf[4], buf[5], buf[6], buf[7],
			buf[8], buf[9], buf[10], buf[11], buf[12], buf[13], buf[14], buf[15])
		targetPort = uint16(buf[16])<<8 | uint16(buf[17])
	default:
		clientConn.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return fmt.Errorf("unsupported address type: %d", atyp)
	}

	targetConn, err := dialer.Dial("tcp", fmt.Sprintf("%s:%d", targetAddr, targetPort))
	if err != nil {
		clientConn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return fmt.Errorf("connect to target %s:%d: %v", targetAddr, targetPort, err)
	}
	defer targetConn.Close()

	_, err = clientConn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	if err != nil {
		return fmt.Errorf("write connect response: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(targetConn, clientConn)
		targetConn.Close()
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, targetConn)
		clientConn.Close()
	}()

	wg.Wait()
	return nil
}

func main() {
	db, err := sql.Open("sqlite3", "proxies.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec("CREATE TABLE IF NOT EXISTS proxies (id INTEGER PRIMARY KEY AUTOINCREMENT, proxy TEXT)")
	if err != nil {
		log.Fatal(err)
	}

	existingProxies := make(map[string]bool)
	var existingRows *sql.Rows
	existingRows, err = db.Query("SELECT proxy FROM proxies")
	if err == nil {
		for existingRows.Next() {
			var proxy string
			if err := existingRows.Scan(&proxy); err == nil {
				existingProxies[proxy] = true
			}
		}
		existingRows.Close()
	}
	log.Printf("Found %d existing proxies in database", len(existingProxies))

	url := "https://raw.githubusercontent.com/ProxyScraper/ProxyScraper/refs/heads/main/socks5.txt"
	resp, err := http.Get(url)
	if err != nil {
		log.Fatalf("Failed to fetch proxy list: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read response: %v", err)
	}

	newProxyList := strings.Split(string(body), "\n")

	allProxies := make(map[string]bool)
	for proxy := range existingProxies {
		allProxies[proxy] = true
	}
	for _, proxy := range newProxyList {
		if proxy != "" {
			allProxies[proxy] = true
		}
	}

	uniqueProxies := make([]string, 0, len(allProxies))
	for proxy := range allProxies {
		uniqueProxies = append(uniqueProxies, proxy)
	}

	log.Printf("Total unique proxies after dedup: %d", len(uniqueProxies))

	_, err = db.Exec("DELETE FROM proxies")
	if err != nil {
		log.Fatalf("Failed to clear table: %v", err)
	}

	results := make(chan string, 500)
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 50)

	log.Println("Testing proxies...")
	testedCount := 0
	successCount := 0

	for _, proxy := range uniqueProxies {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(p string) {
			defer func() { <-semaphore }()
			testProxy(p, &wg, results)
		}(proxy)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for result := range results {
		testedCount++
		if result != "" {
			successCount++
			_, err = db.Exec("INSERT INTO proxies (proxy) VALUES (?)", result)
		}
	}

	log.Printf("Testing complete: %d/%d proxies available", successCount, testedCount)

	startProxyServer(db)
}
