package main

import (
    "bytes"
    "encoding/base64"
    "encoding/json"
    "flag"
    "fmt"
    "io"
    "log"
    "mime/multipart"
    "net/http"
    "net/url"
    "os"
    "path/filepath"
    "runtime"
    "regexp"
    "strings"
    "sync"
    "time"

    "github.com/gorilla/websocket"
)

type Config struct {
    ListenHTTP         string `json:"listen_http"`
    ListenWSPath       string `json:"listen_ws_path"`
    UpstreamWSURL      string `json:"upstream_ws_url"`
    UpstreamAccessToken string `json:"upstream_access_token"`
    UpstreamUseQueryToken bool   `json:"upstream_use_query_token"`
    ServerAccessToken   string `json:"server_access_token"`
    UploadEndpoint     string `json:"upload_endpoint"`
}

type oneBotCommand struct {
    Action string      `json:"action"`
    Params interface{} `json:"params"`
    Echo   interface{} `json:"echo"`
}

type uploadPrivateFileParams struct {
    UserID int64  `json:"user_id"`
    File   string `json:"file"`
    Name   string `json:"name"`
}

type uploadGroupFileParams struct {
    GroupID int64  `json:"group_id"`
    File    string `json:"file"`
    Name    string `json:"name"`
}

type sendPrivateMsgParams struct {
    UserID  int64  `json:"user_id"`
    Message string `json:"message"`
}

type sendGroupMsgParams struct {
    GroupID int64  `json:"group_id"`
    Message string `json:"message"`
}

// cqMediaKinds are CQ types we rewrite for cross-machine sending
var cqMediaKinds = map[string]bool{"image": true, "record": true}

// rewriteCQMediaInText scans CQ codes in text and rewrites media file/path/base64 to remote URL
func rewriteCQMediaInText(s string, cfg *Config) string {
    re := regexp.MustCompile(`\[CQ:(image|record)([^\]]*)]`)
    return re.ReplaceAllStringFunc(s, func(seg string) string {
        // parse key=value pairs
        m := re.FindStringSubmatch(seg)
        if len(m) < 3 { return seg }
        kind := m[1]
        argsStr := m[2]
        args := map[string]string{}
        for _, kv := range strings.Split(strings.TrimLeft(argsStr, ","), ",") {
            if kv == "" { continue }
            parts := strings.SplitN(kv, "=", 2)
            if len(parts) == 2 {
                args[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
            }
        }
        // prefer url if present and http(s)
        if u, ok := args["url"]; ok {
            if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
                return seg
            }
        }
        file := args["file"]
        if file == "" { return seg }
        // if already http(s), keep
        if strings.HasPrefix(file, "http://") || strings.HasPrefix(file, "https://") {
            return seg
        }
        // upload via b, get URL
        up, name := uploadViaB(file, args["name"], cfg)
        if up.URL == "" { return seg }
        args["file"] = escapeCommaMaybe(up.URL)
        if name != "" { args["name"] = name }
        // rebuild CQ segment
        var b strings.Builder
        b.WriteString("[CQ:")
        b.WriteString(kind)
        first := true
        // preserve common argument order: file first
        if v, ok := args["file"]; ok {
            b.WriteString(",file=")
            b.WriteString(v)
            first = false
        }
        for k, v := range args {
            if k == "file" { continue }
            if first {
                b.WriteString(",")
                first = false
            } else {
                b.WriteString(",")
            }
            b.WriteString(k)
            b.WriteString("=")
            b.WriteString(v)
        }
        b.WriteString("]")
        return b.String()
    })
}

var upgrader = websocket.Upgrader{
    ReadBufferSize:  64 * 1024,
    WriteBufferSize: 64 * 1024,
    CheckOrigin: func(r *http.Request) bool { return true },
}

func loadConfig(path string) (*Config, error) {
    f, err := os.Open(path)
    if err != nil {
        return nil, err
    }
    defer f.Close()
    var cfg Config
    dec := json.NewDecoder(f)
    if err := dec.Decode(&cfg); err != nil {
        return nil, err
    }
    if cfg.ListenHTTP == "" {
        cfg.ListenHTTP = ":8081"
    }
    if cfg.ListenWSPath == "" {
        cfg.ListenWSPath = "/ws"
    }
    return &cfg, nil
}

func main() {
    var cfgPath string
    flag.StringVar(&cfgPath, "config", "config.json", "config path")
    flag.Parse()

    cfg, err := loadConfig(cfgPath)
    if err != nil {
        log.Fatalf("load config: %v", err)
    }

    http.HandleFunc(cfg.ListenWSPath, func(w http.ResponseWriter, r *http.Request) {
        // Optional server-side token validation per OneBot v11 forward WS
        if cfg.ServerAccessToken != "" {
            auth := r.Header.Get("Authorization")
            expected := "Bearer " + cfg.ServerAccessToken
            if auth != expected {
                w.WriteHeader(http.StatusUnauthorized)
                _, _ = w.Write([]byte("unauthorized"))
                return
            }
        }
        // Accept WS from sealdice-core adapter
        clientConn, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
            log.Printf("upgrade error: %v", err)
            return
        }

        // Connect to upstream go-cqhttp
        header := http.Header{}
        if cfg.UpstreamAccessToken != "" && !cfg.UpstreamUseQueryToken {
            header.Set("Authorization", "Bearer "+cfg.UpstreamAccessToken)
        }
        upstreamURL := cfg.UpstreamWSURL
        if cfg.UpstreamAccessToken != "" && cfg.UpstreamUseQueryToken {
            if u, err := url.Parse(upstreamURL); err == nil {
                q := u.Query()
                q.Set("access_token", cfg.UpstreamAccessToken)
                u.RawQuery = q.Encode()
                upstreamURL = u.String()
            }
        }
        upstreamConn, _, err := websocket.DefaultDialer.Dial(upstreamURL, header)
        if err != nil {
            log.Printf("upstream dial error: %v", err)
            clientConn.Close()
            return
        }

        var wg sync.WaitGroup
        wg.Add(2)

        // Client -> Upstream
        go func() {
            defer wg.Done()
            for {
                mt, msg, err := clientConn.ReadMessage()
                if err != nil {
                    _ = upstreamConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), timeNowPlus())
                    return
                }
                if mt == websocket.TextMessage {
                    rewritten := rewriteIfUpload(cmdBytes(msg), cfg)
                    msg = rewritten
                }
                if err := upstreamConn.WriteMessage(mt, msg); err != nil {
                    return
                }
            }
        }()

        // Upstream -> Client
        go func() {
            defer wg.Done()
            for {
                mt, msg, err := upstreamConn.ReadMessage()
                if err != nil {
                    _ = clientConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), timeNowPlus())
                    return
                }
                if err := clientConn.WriteMessage(mt, msg); err != nil {
                    return
                }
            }
        }()

        // Wait for both loops
        wg.Wait()
        clientConn.Close()
        upstreamConn.Close()
    })

    log.Printf("middleware-a listening on %s%s, proxying to %s", cfg.ListenHTTP, cfg.ListenWSPath, cfg.UpstreamWSURL)
    if err := http.ListenAndServe(cfg.ListenHTTP, nil); err != nil {
        log.Fatal(err)
    }
}

func timeNowPlus() (deadline time.Time) { // minimal helper to satisfy control writes
    return time.Now().Add(1 * time.Second)
}

func cmdBytes(b []byte) []byte { return b }

func rewriteIfUpload(msg []byte, cfg *Config) []byte {
    // Attempt to parse JSON command
    var cmd oneBotCommand
    if err := json.Unmarshal(msg, &cmd); err != nil {
        return msg
    }
    switch cmd.Action {
    case "send_private_msg":
        raw, _ := json.Marshal(cmd.Params)
        var p map[string]interface{}
        if json.Unmarshal(raw, &p) == nil {
            if v, ok := p["message"].(string); ok {
                nv := rewriteCQMediaInText(v, cfg)
                if nv != v {
                    newCmd := oneBotCommand{Action: "send_private_msg", Params: sendPrivateMsgParams{UserID: int64(p["user_id"].(float64)), Message: nv}, Echo: cmd.Echo}
                    b, _ := json.Marshal(newCmd)
                    return b
                }
            }
        }
        return msg
    case "send_group_msg":
        raw, _ := json.Marshal(cmd.Params)
        var p map[string]interface{}
        if json.Unmarshal(raw, &p) == nil {
            if v, ok := p["message"].(string); ok {
                nv := rewriteCQMediaInText(v, cfg)
                if nv != v {
                    newCmd := oneBotCommand{Action: "send_group_msg", Params: sendGroupMsgParams{GroupID: int64(p["group_id"].(float64)), Message: nv}, Echo: cmd.Echo}
                    b, _ := json.Marshal(newCmd)
                    return b
                }
            }
        }
        return msg
    case "upload_private_file":
        // params must be uploadPrivateFileParams
        raw, _ := json.Marshal(cmd.Params)
        var p uploadPrivateFileParams
        if err := json.Unmarshal(raw, &p); err != nil {
            return msg
        }
        up, name := uploadViaB(p.File, p.Name, cfg)
        if up.LocalPath != "" {
            // Keep original upload action, rewrite to remote local path
            newCmd := oneBotCommand{
                Action: "upload_private_file",
                Params: uploadPrivateFileParams{UserID: p.UserID, File: up.LocalPath, Name: name},
                Echo:   cmd.Echo,
            }
            b, _ := json.Marshal(newCmd)
            return b
        }
        if up.URL != "" {
            // Fallback: send as message with CQ:file URL
            cq := fmt.Sprintf("[CQ:file,file=%s,name=%s]", escapeCommaMaybe(up.URL), name)
            newCmd := oneBotCommand{
                Action: "send_private_msg",
                Params: sendPrivateMsgParams{UserID: p.UserID, Message: cq},
                Echo:   cmd.Echo,
            }
            b, _ := json.Marshal(newCmd)
            return b
        }
        return msg
    case "upload_group_file":
        raw, _ := json.Marshal(cmd.Params)
        var p uploadGroupFileParams
        if err := json.Unmarshal(raw, &p); err != nil {
            return msg
        }
        up, name := uploadViaB(p.File, p.Name, cfg)
        if up.LocalPath != "" {
            newCmd := oneBotCommand{
                Action: "upload_group_file",
                Params: uploadGroupFileParams{GroupID: p.GroupID, File: up.LocalPath, Name: name},
                Echo:   cmd.Echo,
            }
            b, _ := json.Marshal(newCmd)
            return b
        }
        if up.URL != "" {
            cq := fmt.Sprintf("[CQ:file,file=%s,name=%s]", escapeCommaMaybe(up.URL), name)
            newCmd := oneBotCommand{
                Action: "send_group_msg",
                Params: sendGroupMsgParams{GroupID: p.GroupID, Message: cq},
                Echo:   cmd.Echo,
            }
            b, _ := json.Marshal(newCmd)
            return b
        }
        return msg
    default:
        return msg
    }
}

func escapeCommaMaybe(text string) string { return strings.ReplaceAll(text, ",", "%2C") }

type uploadResult struct {
    URL       string
    LocalPath string
}

func uploadViaB(fileField string, name string, cfg *Config) (uploadResult, string) {
    path := fileField
    // If already an HTTP(S) URL, return directly
    if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
        if name == "" {
            if u, err := url.Parse(path); err == nil {
                base := filepath.Base(u.Path)
                if base != "" && base != "/" {
                    name = base
                }
            }
        }
        return uploadResult{URL: path}, name
    }
    // Handle base64:// content
    if strings.HasPrefix(path, "base64://") {
        enc := strings.TrimPrefix(path, "base64://")
        // support optional data URI header like data:...;base64,xxxx
        if idx := strings.IndexByte(enc, ','); idx != -1 {
            enc = enc[idx+1:]
        }
        data, err := base64.StdEncoding.DecodeString(enc)
        if err != nil {
            log.Printf("decode base64 failed: %v", err)
            return uploadResult{}, ""
        }
        if name == "" {
            name = "file.bin"
        }
        var body bytes.Buffer
        writer := multipart.NewWriter(&body)
        part, err := writer.CreateFormFile("file", name)
        if err != nil {
            log.Printf("create form file failed: %v", err)
            return uploadResult{}, ""
        }
        if _, err := part.Write(data); err != nil {
            log.Printf("write base64 data failed: %v", err)
            return uploadResult{}, ""
        }
        _ = writer.WriteField("name", name)
        writer.Close()

        req, err := http.NewRequest("POST", cfg.UploadEndpoint, &body)
        if err != nil {
            log.Printf("new request failed: %v", err)
            return uploadResult{}, ""
        }
        req.Header.Set("Content-Type", writer.FormDataContentType())
        resp, err := http.DefaultClient.Do(req)
        if err != nil {
            log.Printf("upload HTTP error: %v", err)
            return uploadResult{}, ""
        }
        defer resp.Body.Close()
        b, _ := io.ReadAll(resp.Body)
        if resp.StatusCode/100 != 2 {
            log.Printf("upload failed status=%d body=%s", resp.StatusCode, string(b))
            return uploadResult{}, ""
        }
        var ret struct {
            URL       string `json:"url"`
            Name      string `json:"name"`
            LocalPath string `json:"local_path"`
        }
        if err := json.Unmarshal(b, &ret); err != nil {
            log.Printf("parse upload response failed: %v", err)
            return uploadResult{}, ""
        }
        if ret.Name != "" {
            name = ret.Name
        }
        return uploadResult{URL: ret.URL, LocalPath: ret.LocalPath}, name
    }
    // Handle file:// URI
    if strings.HasPrefix(path, "file://") {
        u, err := url.Parse(path)
        if err == nil {
            path = u.Path
            if runtime.GOOS == "windows" && strings.HasPrefix(path, "/") {
                // drop leading slash for windows drive path
                if len(path) >= 3 && path[2] == ':' {
                    path = path[1:]
                } else {
                    path = strings.TrimPrefix(path, "/")
                }
            }
        }
    }
    // If still not absolute, try to make absolute
    if !filepath.IsAbs(path) {
        abs, err := filepath.Abs(path)
        if err == nil {
            path = abs
        }
    }
    f, err := os.Open(path)
    if err != nil {
        log.Printf("open file for upload failed: %v", err)
        return uploadResult{}, ""
    }
    defer f.Close()
    if name == "" {
        name = filepath.Base(path)
    }

    // Build multipart form
    var body bytes.Buffer
    writer := multipart.NewWriter(&body)
    part, err := writer.CreateFormFile("file", name)
    if err != nil {
        log.Printf("create form file failed: %v", err)
        return uploadResult{}, ""
    }
    if _, err := io.Copy(part, f); err != nil {
        log.Printf("copy file failed: %v", err)
        return uploadResult{}, ""
    }
    _ = writer.WriteField("name", name)
    writer.Close()

    req, err := http.NewRequest("POST", cfg.UploadEndpoint, &body)
    if err != nil {
        log.Printf("new request failed: %v", err)
        return uploadResult{}, ""
    }
    req.Header.Set("Content-Type", writer.FormDataContentType())
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        log.Printf("upload HTTP error: %v", err)
        return uploadResult{}, ""
    }
    defer resp.Body.Close()
    b, _ := io.ReadAll(resp.Body)
    if resp.StatusCode/100 != 2 {
        log.Printf("upload failed status=%d body=%s", resp.StatusCode, string(b))
        return uploadResult{}, ""
    }
    var ret struct {
        URL       string `json:"url"`
        Name      string `json:"name"`
        LocalPath string `json:"local_path"`
    }
    if err := json.Unmarshal(b, &ret); err != nil {
        log.Printf("parse upload response failed: %v", err)
        return uploadResult{}, ""
    }
    if ret.Name != "" {
        name = ret.Name
    }
    return uploadResult{URL: ret.URL, LocalPath: ret.LocalPath}, name
}