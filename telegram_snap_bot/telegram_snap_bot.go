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
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	BotToken    string
	ChatID      int64
	SnapURL     string
	DescribeCmd string
	PicoclaCmd  string
}

func mustenv(k string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		panic("missing env: " + k)
	}
	return v
}

func getenv(k, d string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return d
	}
	return v
}

// ---------------------------------------------------------------------------
// Telegram types
// ---------------------------------------------------------------------------

type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *TGMessage     `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type TGMessage struct {
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

type CallbackQuery struct {
	ID      string     `json:"id"`
	Data    string     `json:"data"`
	Message *TGMessage `json:"message"`
	From    struct {
		ID int64 `json:"id"`
	} `json:"from"`
}

type UpdatesResponse struct {
	Ok     bool     `json:"ok"`
	Result []Update `json:"result"`
}

type DescribeResult struct {
	Ok          bool   `json:"ok"`
	Description string `json:"description"`
	Error       string `json:"error"`
}

// ---------------------------------------------------------------------------
// Conversation state
// ---------------------------------------------------------------------------

// PendingAction guarda la acción esperando confirmación del usuario.
type PendingAction struct {
	Instruction string
	Description string
}

var (
	pendingMu sync.Mutex
	pending   = map[int64]*PendingAction{}
)

func setPending(chatID int64, a *PendingAction) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	pending[chatID] = a
}

func popPending(chatID int64) *PendingAction {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	a := pending[chatID]
	delete(pending, chatID)
	return a
}

// waitingGoal: el chat espera que el usuario escriba el objetivo de /auto.
var (
	waitingGoalMu sync.Mutex
	waitingGoal   = map[int64]bool{}
)

func setWaitingGoal(chatID int64, v bool) {
	waitingGoalMu.Lock()
	defer waitingGoalMu.Unlock()
	waitingGoal[chatID] = v
}

func isWaitingGoal(chatID int64) bool {
	waitingGoalMu.Lock()
	defer waitingGoalMu.Unlock()
	return waitingGoal[chatID]
}

// ---------------------------------------------------------------------------
// Telegram API helpers
// ---------------------------------------------------------------------------

func getUpdates(token string, offset int) ([]Update, error) {
	url := fmt.Sprintf(
		"https://api.telegram.org/bot%s/getUpdates?timeout=20&offset=%d",
		token, offset,
	)
	client := http.Client{Timeout: 25 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r UpdatesResponse
	json.NewDecoder(resp.Body).Decode(&r)
	return r.Result, nil
}

func sendMessage(cfg Config, text string) {
	payload := map[string]any{
		"chat_id": cfg.ChatID,
		"text":    text,
	}
	b, _ := json.Marshal(payload)
	http.Post(
		"https://api.telegram.org/bot"+cfg.BotToken+"/sendMessage",
		"application/json",
		bytes.NewReader(b),
	)
}

// sendConfirmation manda mensaje con botones inline Confirmar / Cancelar.
func sendConfirmation(cfg Config, text string) {
	payload := map[string]any{
		"chat_id": cfg.ChatID,
		"text":    text,
		"reply_markup": map[string]any{
			"inline_keyboard": [][]map[string]any{
				{
					{"text": "✅ Confirmar", "callback_data": "confirm"},
					{"text": "❌ Cancelar", "callback_data": "cancel"},
				},
			},
		},
	}
	b, _ := json.Marshal(payload)
	http.Post(
		"https://api.telegram.org/bot"+cfg.BotToken+"/sendMessage",
		"application/json",
		bytes.NewReader(b),
	)
}

// answerCallbackQuery quita el spinner del botón inline.
func answerCallbackQuery(cfg Config, callbackID string) {
	payload := map[string]any{"callback_query_id": callbackID}
	b, _ := json.Marshal(payload)
	http.Post(
		"https://api.telegram.org/bot"+cfg.BotToken+"/answerCallbackQuery",
		"application/json",
		bytes.NewReader(b),
	)
}

func sendPhoto(cfg Config, img []byte) {
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	writer.WriteField("chat_id", strconv.FormatInt(cfg.ChatID, 10))
	part, _ := writer.CreateFormFile("photo", "snap.jpg")
	part.Write(img)
	writer.Close()
	req, _ := http.NewRequest(
		"POST",
		"https://api.telegram.org/bot"+cfg.BotToken+"/sendPhoto",
		body,
	)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	http.DefaultClient.Do(req)
}

// ---------------------------------------------------------------------------
// Domain helpers
// ---------------------------------------------------------------------------

func snapshot(url string) ([]byte, error) {
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func describeScreen(cfg Config) (string, error) {
	cmd := exec.Command(cfg.DescribeCmd)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("describe cmd: %w", err)
	}
	var r DescribeResult
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("describe parse: %w", err)
	}
	if !r.Ok {
		return "", fmt.Errorf("%s", r.Error)
	}
	return r.Description, nil
}

func runPicoclaw(cfg Config, description, instruction string) error {
	msg := fmt.Sprintf(
		"Screen context: %s\n\nInstruction: %s",
		description,
		instruction,
	)

	model := getenv("PICOCLAW_MODEL", "gpt-5.2")
	home := getenv("PICOCLAW_HOME", "/root")

	cmd := exec.Command(
		cfg.PicoclawCmd,
		"agent",
		"--model", model,
		"--message", msg,
	)

	cmd.Env = append(os.Environ(),
		"HOME="+home,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("picoclaw: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Handlers (goroutines para no bloquear el polling loop)
// ---------------------------------------------------------------------------

var verRunning atomic.Bool

// handleVer: captura + describe, sin acción sobre hardware.
func handleVer(cfg Config, wg *sync.WaitGroup) {
	defer wg.Done()
	defer verRunning.Store(false)

	img, err := snapshot(cfg.SnapURL)
	if err != nil {
		sendMessage(cfg, "❌ snapshot: "+err.Error())
		return
	}
	sendPhoto(cfg, img)

	desc, err := describeScreen(cfg)
	if err != nil {
		sendMessage(cfg, "❌ describe: "+err.Error())
		return
	}
	sendMessage(cfg, desc)
}

// handleHacer: captura, describe y pide confirmación antes de actuar.
func handleHacer(cfg Config, instruction string, wg *sync.WaitGroup) {
	defer wg.Done()

	sendMessage(cfg, "🔍 Analizando pantalla...")

	desc, err := describeScreen(cfg)
	if err != nil {
		sendMessage(cfg, "❌ describe: "+err.Error())
		return
	}

	// Guardar estado pendiente ANTES de pedir confirmación.
	setPending(cfg.ChatID, &PendingAction{
		Instruction: instruction,
		Description: desc,
	})

	confirmText := fmt.Sprintf(
		"👁 Veo en pantalla:\n%s\n\n⚡ Instrucción a ejecutar:\n%s\n\n¿Confirmás?",
		desc, instruction,
	)
	sendConfirmation(cfg, confirmText)
}

// handleConfirm: ejecuta la acción pendiente tras confirmación del usuario.
func handleConfirm(cfg Config, wg *sync.WaitGroup) {
	defer wg.Done()

	action := popPending(cfg.ChatID)
	if action == nil {
		sendMessage(cfg, "⚠️ No hay ninguna acción pendiente.")
		return
	}

	sendMessage(cfg, "⚙️ Ejecutando con PicoClaw...")

	if err := runPicoclaw(cfg, action.Description, action.Instruction); err != nil {
		sendMessage(cfg, "❌ "+err.Error())
		return
	}
	sendMessage(cfg, "✅ Acción ejecutada.")
}

// ---------------------------------------------------------------------------
// Main loop
// ---------------------------------------------------------------------------

func main() {
	cfg := Config{
		BotToken:    mustenv("TG_BOT_TOKEN"),
		SnapURL:     getenv("SNAPSHOT_URL", "http://127.0.0.1:18080/snapshot.jpg"),
		DescribeCmd: getenv("DESCRIBE_CMD", "/root/nk-agent/bin/picoclaw-see-json.sh"),
		PicoclaCmd:  getenv("PICOCLAW_CMD", "/bin/picoclaw/picoclaw-linux-riscv64"),
	}
	id, _ := strconv.ParseInt(mustenv("TG_CHAT_ID"), 10, 64)
	cfg.ChatID = id

	var (
		offset int
		wg     sync.WaitGroup
	)

	for {
		updates, err := getUpdates(cfg.BotToken, offset)
		if err != nil {
			fmt.Fprintf(os.Stderr, "getUpdates: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		for _, u := range updates {
			// Confirmar offset SIEMPRE e INMEDIATAMENTE antes de procesar.
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}

			// --- Callback de botón inline (Confirmar / Cancelar) ---
			if u.CallbackQuery != nil {
				cq := u.CallbackQuery
				// Solo atender callbacks del usuario autorizado.
				if cq.From.ID != cfg.ChatID {
					answerCallbackQuery(cfg, cq.ID)
					continue
				}
				answerCallbackQuery(cfg, cq.ID)

				switch cq.Data {
				case "confirm":
					wg.Add(1)
					go handleConfirm(cfg, &wg)
				case "cancel":
					popPending(cfg.ChatID)
					sendMessage(cfg, "❌ Acción cancelada.")
				}
				continue
			}

			// --- Mensaje de texto ---
			if u.Message == nil {
				continue
			}
			if u.Message.Chat.ID != cfg.ChatID {
				continue
			}

			text := strings.TrimSpace(u.Message.Text)
			lower := strings.ToLower(text)

			// Si /auto está esperando el objetivo, cualquier texto es la instrucción.
			if isWaitingGoal(cfg.ChatID) {
				setWaitingGoal(cfg.ChatID, false)
				wg.Add(1)
				go handleHacer(cfg, text, &wg)
				continue
			}

			switch {
			case lower == "/snap":
				img, err := snapshot(cfg.SnapURL)
				if err != nil {
					sendMessage(cfg, "❌ snapshot: "+err.Error())
					continue
				}
				sendPhoto(cfg, img)

			case lower == "/ver":
				if !verRunning.CompareAndSwap(false, true) {
					sendMessage(cfg, "⏳ Ya hay un análisis en curso.")
					continue
				}
				wg.Add(1)
				go handleVer(cfg, &wg)

			case strings.HasPrefix(lower, "/hacer "):
				// Preservar casing original de la instrucción.
				instruction := strings.TrimSpace(text[len("/hacer "):])
				wg.Add(1)
				go handleHacer(cfg, instruction, &wg)

			case lower == "/hacer":
				sendMessage(cfg, "⚠️ Uso: /hacer <instrucción>\nEjemplo: /hacer clic en Aceptar")

			case lower == "/auto":
				setWaitingGoal(cfg.ChatID, true)
				sendMessage(cfg, "🤖 ¿Qué quieres hacer? Describe el objetivo:")
			}
		}
	}
}