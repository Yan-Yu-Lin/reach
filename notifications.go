package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

func (s *Server) notifyPendingRequest(requestID, name string, metadata map[string]any) {
	cfg := s.cfg.Notifications
	if !cfg.Enabled || strings.TrimSpace(cfg.PendingRequest.WebhookURL) == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.sendPendingRequestWebhook(ctx, requestID, name, metadata); err != nil {
			log.Printf("pending request notification failed: %v", err)
		}
	}()
}

func (s *Server) sendPendingRequestWebhook(ctx context.Context, requestID, name string, metadata map[string]any) error {
	var body strings.Builder
	fmt.Fprintf(&body, "New Reach access request: %s\n", firstNonEmpty(name, "unknown"))
	fmt.Fprintf(&body, "Request: %s\n", requestID)
	if v := metadataString(metadata, "source_ip"); v != "" {
		fmt.Fprintf(&body, "Source IP: %s\n", v)
	}
	if v := metadataString(metadata, "target_user"); v != "" {
		fmt.Fprintf(&body, "Target user: %s\n", v)
	}
	if distro := metadataString(metadata, "distro"); distro != "" {
		arch := metadataString(metadata, "arch")
		if arch != "" {
			fmt.Fprintf(&body, "Target: %s / %s\n", distro, arch)
		} else {
			fmt.Fprintf(&body, "Target: %s\n", distro)
		}
	}
	if s.cfg.DefaultHub.APIURL != "" {
		fmt.Fprintf(&body, "Dashboard: %s\n", strings.TrimRight(s.cfg.DefaultHub.APIURL, "/"))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Notifications.PendingRequest.WebhookURL, bytes.NewBufferString(body.String()))
	if err != nil {
		return err
	}
	setHeaderIfMissing(req, "Title", "Reach pending request")
	setHeaderIfMissing(req, "Priority", "high")
	setHeaderIfMissing(req, "Tags", "computer,warning")
	if s.cfg.DefaultHub.APIURL != "" {
		setHeaderIfMissing(req, "Click", strings.TrimRight(s.cfg.DefaultHub.APIURL, "/"))
	}
	for k, v := range s.cfg.Notifications.PendingRequest.WebhookHeaders {
		if strings.TrimSpace(k) != "" {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", res.Status)
	}
	return nil
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	v, ok := metadata[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func setHeaderIfMissing(req *http.Request, key, value string) {
	if req.Header.Get(key) == "" {
		req.Header.Set(key, value)
	}
}
