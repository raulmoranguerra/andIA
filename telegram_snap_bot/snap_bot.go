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
	PicoclawPath string
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

func saveTempSnapshot(jpeg []byte) (string, error) {
	path := "/tmp/telegram-ver-snapshot.jpg"
	err := os.WriteFile(path, jpeg, 0600)
	return path, err
}

func describeImage(cfg Config, imgPath string) (string, error) {
	cmd := exec.Command(cfg.DescribeCmd, imgPath)
	cmd.Env = append(os.Environ(), "PICOCLAW_PATH="+cfg.PicoclawPath)

	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))

	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, err
	}

	if text == "" {
		return "Picoclaw no devolvió ninguna descripción.", nil
	}

	return text, nil
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

	imgPath, err := saveTempSnapshot(jpeg)
	if err != nil {
		_ = sendMessage(cfg, "No pude guardar la captura temporal: "+err.Error())
		return
	}

	desc, err := describeImage(cfg, imgPath)
	if err != nil {
		_ = sendMessage(cfg, "Error describiendo la imagen: "+desc)
		return
	}

	_ = sendMessage(cfg, desc)
}

func main() {
	cfg := Config{
		BotToken:     mustEnv("TG_BOT_TOKEN"),
		SnapshotURL:  getenv("SNAPSHOT_URL", "http://127.0.0.1:18080/snapshot.jpg"),
		PicoclawPath: getenv("PICOCLAW_PATH", "/bin/picoclaw-cli"),
		DescribeCmd:  getenv("DESCRIBE_CMD", "/root/nk-agent/describe-image.sh"),
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

			if update.Message == nil {
				continue
			}

			if update.Message.Chat.ID != cfg.ChatID {
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