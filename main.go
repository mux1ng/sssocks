package main

import (
    "database/sql"
    "fmt"
    "io"
    "io/ioutil"
    "log"
    "net"
    "net/http"
    "net/url"
    "os"
    "strings"
    "sync"
    "time"

    proxy1 "golang.org/x/net/proxy"
    _ "github.com/mattn/go-sqlite3"
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

    // 创建一个 Dialer 来检测代理
    dialer := &net.Dialer{
        Timeout:   2 * time.Second,
        KeepAlive: 2 * time.Second,
    }

    // 设置 SOCKS5 代理
    transport := &http.Transport{
        Proxy: http.ProxyURL(&url.URL{
            Scheme: "socks5",
            Host:   fmt.Sprintf("%s:%s", host, port),
        }),
        DialContext: dialer.DialContext,
    }

    client := &http.Client{
        Transport: transport,
        Timeout:   10 * time.Second,
    }

    // 尝试连接公共网站
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
    defer listener.Close()
    log.Println("Starting proxy server on :8088")

    for {
        conn, err := listener.Accept()
        if err != nil {
            log.Printf("Failed to accept connection: %v", err)
            continue
        }
        go handleConnection(conn, db)
    }
}

// 处理代理连接
func handleConnection(conn net.Conn, db *sql.DB) {
    defer conn.Close()

    // 从数据库中随机选择一个可用代理
    row := db.QueryRow("SELECT proxy FROM proxies ORDER BY RANDOM() LIMIT 1")
    var proxy string
    err := row.Scan(&proxy)
    if err != nil {
        log.Printf("No available proxy: %v", err)
        return
    }

    parts := strings.Split(proxy, ":")
    if len(parts) != 2 {
        log.Println("Invalid proxy format")
        return
    }

    host := parts[0]
    port := parts[1]

    // 使用 golang.org/x/net/proxy 创建 SOCKS5 代理客户端
    dialer, err := proxy1.SOCKS5("tcp", fmt.Sprintf("%s:%s", host, port), nil, proxy1.Direct)
    if err != nil {
        log.Printf("Failed to connect to SOCKS5 proxy %s:%s: %v", host, port, err)
        return
    }

    // 连接到目标服务器
    targetConn, err := dialer.Dial("tcp", conn.RemoteAddr().String())
    if err != nil {
        log.Printf("Failed to connect to target: %v", err)
        return
    }
    defer targetConn.Close()

    // 启动两个 goroutine 来转发流量
    go func() {
        // 转发客户端请求到目标服务器，并记录流量
        _, err := io.Copy(targetConn, conn)
        if err != nil {
            log.Printf("Error forwarding request to target: %v", err)
        }
    }()

    _, err = io.Copy(conn, targetConn)
    if err != nil {
        log.Printf("Error forwarding response to client: %v", err)
    }
}

func main() {
    // 删除已存在的 proxies.db
    if _, err := os.Stat("proxies.db"); err == nil {
        err = os.Remove("proxies.db")
        if err != nil {
            fmt.Printf("Failed to remove existing proxies.db: %v\n", err)
            return
        }
    }

    url := "https://raw.githubusercontent.com/ProxyScraper/ProxyScraper/refs/heads/main/socks5.txt"
    resp, err := http.Get(url)
    if err != nil {
        fmt.Printf("获取代理列表失败: %v\n", err)
        return
    }
    defer resp.Body.Close()

    body, err := ioutil.ReadAll(resp.Body)
    if err != nil {
        fmt.Printf("读取代理列表失败: %v\n", err)
        return
    }

    proxyList := strings.Split(string(body), "\n")
    results := make(chan string, len(proxyList))
    var wg sync.WaitGroup
    // 限制并发数量为 50
    semaphore := make(chan struct{}, 50)

    // 打开或创建 SQLite 数据库
    db, err := sql.Open("sqlite3", "proxies.db")
    if err != nil {
        fmt.Println(err)
        return
    }
    defer db.Close()

    // 创建表
    _, err = db.Exec("CREATE TABLE IF NOT EXISTS proxies (id INTEGER PRIMARY KEY AUTOINCREMENT, proxy TEXT)")
    if err != nil {
        fmt.Println(err)
        return
    }

    // 测试代理
    for _, proxy := range proxyList {
        if proxy == "" {
            continue
        }
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

    // 插入可用代理
    for result := range results {
        if result != "" {
            _, err = db.Exec("INSERT INTO proxies (proxy) VALUES (?)", result)
            if err != nil {
                fmt.Printf("插入代理 %s 失败: %v\n", result, err)
            } else {
                fmt.Printf("插入代理 %s 成功\n", result)
            }
        }
    }

    // 启动代理服务器
    startProxyServer(db)
}
