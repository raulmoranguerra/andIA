package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	BotToken     string
	ChatID       int64
	SnapshotURL  string
	DescribeCmd  string
}

type TelegramResponse struct {
	Ok     bool `json:"ok"`
	Result []struct {
		UpdateID int `json:"update_id"`
		Message  *struct {
			Text string `json:"text"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"result"`
}

type DescribeResult struct {
	Ok          bool   `json:"ok"`
	ImagePath   string `json:"image_path"`
	Description string `json:"description"`
	Error       string `json:"error"`
}

func mustEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing env var: %s\n", key)
		os.Exit(1)
	}
	return v
}

func getenv(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func getSnapshot(url string) ([]byte, string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("snapshot status=%d body=%s", resp.StatusCode, string(body))
	}

	contentType := resp.Header.Get("Content-Type")
	data, err := io.ReadAll(resp.Body)
	return data, contentType, err
}

func sendMessage(cfg Config, text string) error {
	payload := map[string]any{
		"chat_id": cfg.ChatID,
		"text":    text,
	}
	b, _ := json.Marshal(payload)

	resp, err := http.Post(
		"https://api.telegram.org/bot"+cfg.BotToken+"/sendMessage",
		"application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sendMessage status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}

func sendPhoto(cfg Config, jpeg []byte, caption string) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	_ = writer.WriteField("chat_id", strconv.FormatInt(cfg.ChatID, 10))
	if caption != "" {
		_ = writer.WriteField("caption", caption)
	}

	part, err := writer.CreateFormFile("photo", "snapshot.jpg")
	if err != nil {
		return err
	}
	if _, err := part.Write(jpeg); err != nil {
		return err
	}
	_ = writer.Close()

	req, err := http.NewRequest(
		"POST",
		"https://api.telegram.org/bot"+cfg.BotToken+"/sendPhoto",
		&body,
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sendPhoto status=%d body=%s", resp.StatusCode, string(respBody))
	}
	return nil
}

func getUpdates(cfg Config, offset int) (TelegramResponse, error) {
	url := fmt.Sprintf(
		"https://api.telegram.org/bot%s/getUpdates?timeout=20&offset=%d",
		cfg.BotToken,
		offset,
	)

	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return TelegramResponse{}, err
	}
	defer resp.Body.Close()

	var tr TelegramResponse
	err = json.NewDecoder(resp.Body).Decode(&tr)
	return tr, err
}

func runDescribeScript(cfg Config) (DescribeResult, error) {
	cmd := exec.Command(cfg.DescribeCmd)
	out, err := cmd.CombinedOutput()

	var result DescribeResult
	parseErr := json.Unmarshal(out, &result)
	if parseErr != nil {
		return DescribeResult{}, fmt.Errorf("script output is not valid JSON: %s", strings.TrimSpace(string(out)))
	}

	if err != nil {
		if result.Error != "" {
			return result, fmt.Errorf(result.Error)
		}
		return result, err
	}

	if !result.Ok {
		if result.Error != "" {
			return result, fmt.Errorf(result.Error)
		}
		return result, fmt.Errorf("unknown describe script error")
	}

	return result, nil
}

func handleSnap(cfg Config) {
	jpeg, contentType, err := getSnapshot(cfg.SnapshotURL)
	if err != nil {
		_ = sendMessage(cfg, "Error obteniendo snapshot: "+err.Error())
		return
	}

	if !strings.Contains(strings.ToLower(contentType), "image/jpeg") &&
		!strings.Contains(strings.ToLower(contentType), "image/") {
		_ = sendMessage(cfg, "El endpoint no devolvió una imagen válida. Content-Type: "+contentType)
		return
	}

	err = sendPhoto(cfg, jpeg, "Captura actual")
	if err != nil {
		_ = sendMessage(cfg, "Error enviando la foto: "+err.Error())
	}
}

func handleVer(cfg Config) {
	jpeg, contentType, err := getSnapshot(cfg.SnapshotURL)
	if err != nil {
		_ = sendMessage(cfg, "Error obteniendo snapshot: "+err.Error())
		return
	}

	if !strings.Contains(strings.ToLower(contentType), "image/jpeg") &&
		!strings.Contains(strings.ToLower(contentType), "image/") {
		_ = sendMessage(cfg, "El endpoint no devolvió una imagen válida. Content-Type: "+contentType)
		return
	}

	_ = sendPhoto(cfg, jpeg, "Captura actual")

	result, err := runDescribeScript(cfg)
	if err != nil {
		_ = sendMessage(cfg, "Error describiendo la imagen: "+err.Error())
		return
	}

	if strings.TrimSpace(result.Description) == "" {
		_ = sendMessage(cfg, "No se obtuvo descripción.")
		return
	}

	_ = sendMessage(cfg, result.Description)
}

func main() {
	cfg := Config{
		BotToken:    mustEnv("TG_BOT_TOKEN"),
		SnapshotURL: getenv("SNAPSHOT_URL", "http://127.0.0.1:18080/snapshot.jpg"),
		DescribeCmd: getenv("DESCRIBE_CMD", "/root/nk-agent/bin/picoclaw-see-json.sh"),
	}

	chatID, err := strconv.ParseInt(mustEnv("TG_CHAT_ID"), 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid TG_CHAT_ID:", err)
		os.Exit(1)
	}
	cfg.ChatID = chatID

	offset := 0

	for {
		updates, err := getUpdates(cfg, offset)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		for _, update := range updates.Result {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}

			if update.Message == nil || update.Message.Chat.ID != cfg.ChatID {
				continue
			}

			text := strings.TrimSpace(strings.ToLower(update.Message.Text))
			switch text {
			case "/snap", "/capture":
				handleSnap(cfg)
			case "/ver":
				handleVer(cfg)
			case "/start":
				_ = sendMessage(cfg, "Bot activo. Usa /snap para captura y /ver para captura + descripción.")
			}
		}

		time.Sleep(700 * time.Millisecond)
	}
}