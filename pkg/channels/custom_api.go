package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

type CustomAPIChannel struct {
	*BaseChannel
	config config.CustomAPIConfig
	server *http.Server
	mu     sync.Mutex
}

func NewCustomAPIChannel(cfg config.CustomAPIConfig, messageBus *bus.MessageBus) (*CustomAPIChannel, error) {
	return &CustomAPIChannel{
		BaseChannel: NewBaseChannel("custom_api", cfg, messageBus, cfg.AllowFrom),
		config:      cfg,
	}, nil
}

func (c *CustomAPIChannel) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.IsRunning() {
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/message", c.handleMessage)

	addr := fmt.Sprintf("0.0.0.0:%d", c.config.Port)
	c.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		logger.InfoCF("channels", "CustomAPI channel listening", map[string]interface{}{
			"port": c.config.Port,
		})
		if err := c.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.ErrorCF("channels", "CustomAPI server error", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}()

	c.setRunning(true)
	return nil
}

func (c *CustomAPIChannel) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.IsRunning() {
		return nil
	}

	if c.server != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := c.server.Shutdown(shutdownCtx); err != nil {
			logger.ErrorCF("channels", "CustomAPI shutdown error", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}

	c.setRunning(false)
	return nil
}

func (c *CustomAPIChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if c.config.WebhookURL == "" {
		logger.InfoCF("channels", "CustomAPI outbound message (no webhook configured)", map[string]interface{}{
			"chat_id": msg.ChatID,
			"content": msg.Content,
		})
		return nil
	}

	payload := map[string]string{
		"chat_id": msg.ChatID,
		"content": msg.Content,
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", c.config.WebhookURL, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.Token)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned status: %d", resp.StatusCode)
	}

	return nil
}

func (c *CustomAPIChannel) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Token verification
	if c.config.Token != "" {
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") || strings.TrimPrefix(authHeader, "Bearer ") != c.config.Token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var req struct {
		SenderID string `json:"sender_id"`
		ChatID   string `json:"chat_id"`
		Content  string `json:"content"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if req.SenderID == "" {
		req.SenderID = "api_user"
	}
	if req.ChatID == "" {
		req.ChatID = "api_chat"
	}

	if !c.IsAllowed(req.SenderID) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Publish to bus
	c.HandleMessage(req.SenderID, req.ChatID, req.Content, nil, nil)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"received"}`))
}
