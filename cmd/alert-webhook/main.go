// Command alert-webhook 接收 Alertmanager 的 webhook 通知，并把每条告警写成一行 JSON。
//
// 用法：
//
//	go run ./cmd/alert-webhook -addr :8099 -log-file /var/log/pjgoiot/alerts.log
//
// Alertmanager 配置见 deploy/alertmanager/alertmanager.yml；本地端到端验证见
// scripts/verify-alerting.sh。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

type alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
	Fingerprint string            `json:"fingerprint"`
}

type payload struct {
	Version      string            `json:"version"`
	Status       string            `json:"status"`
	GroupKey     string            `json:"groupKey"`
	CommonLabels map[string]string `json:"commonLabels"`
	Alerts       []alert           `json:"alerts"`
}

type record struct {
	ReceivedAt  time.Time `json:"receivedAt"`
	Status      string    `json:"status"`
	AlertName   string    `json:"alertname"`
	Severity    string    `json:"severity"`
	Service     string    `json:"service"`
	Environment string    `json:"env"`
	Fingerprint string    `json:"fingerprint"`
	Summary     string    `json:"summary"`
	StartsAt    time.Time `json:"startsAt"`
	EndsAt      time.Time `json:"endsAt"`
}

func main() {
	addr := flag.String("addr", envOr("ALERT_WEBHOOK_ADDR", ":8099"), "监听地址")
	logFile := flag.String("log-file", os.Getenv("ALERT_WEBHOOK_LOG"), "把接收到的告警追加写入该文件（空则只输出到 stdout）")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		var p payload
		if err := json.Unmarshal(body, &p); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		now := time.Now().UTC()
		for _, a := range p.Alerts {
			rec := record{
				ReceivedAt:  now,
				Status:      a.Status,
				AlertName:   a.Labels["alertname"],
				Severity:    a.Labels["severity"],
				Service:     a.Labels["service"],
				Environment: a.Labels["env"],
				Fingerprint: a.Fingerprint,
				Summary:     a.Annotations["summary"],
				StartsAt:    a.StartsAt,
				EndsAt:      a.EndsAt,
			}
			line, err := json.Marshal(rec)
			if err != nil {
				log.Printf("序列化告警失败: %v", err)
				continue
			}
			fmt.Println(string(line))
			if *logFile != "" {
				f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
				if err == nil {
					_, _ = f.Write(append(line, '\n'))
					_ = f.Close()
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"received":%d}`, len(p.Alerts))
	})

	log.Printf("alert-webhook 监听 %s", *addr)
	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("alert-webhook 退出: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
