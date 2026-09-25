package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func baseURL() string {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":9467"
	}
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr
	}
	return "http://" + addr
}

func runCLI(args []string) {
	if len(args) == 0 {
		printCLIHelp()
		os.Exit(2)
	}
	cmd := args[0]
	switch cmd {
	case "help", "-h", "--help":
		printCLIHelp()
	case "health":
		cliGET("/health")
	case "telegram":
		cliGET("/debug/telegram")
	case "ping":
		cliGET("/debug/telegram?ping=1")
	case "diun":
		image := ""
		digest := fmt.Sprintf("sha256:manual-%d", time.Now().Unix())
		container := ""
		for i := 1; i < len(args); i++ {
			a := args[i]
			switch {
			case strings.HasPrefix(a, "--image="):
				image = strings.TrimPrefix(a, "--image=")
			case a == "--image" && i+1 < len(args):
				i++
				image = args[i]
			case strings.HasPrefix(a, "--digest="):
				digest = strings.TrimPrefix(a, "--digest=")
			case a == "--digest" && i+1 < len(args):
				i++
				digest = args[i]
			case strings.HasPrefix(a, "--container="):
				container = strings.TrimPrefix(a, "--container=")
			case a == "--container" && i+1 < len(args):
				i++
				container = args[i]
			}
		}
		if image == "" || container == "" {
			fmt.Fprintln(os.Stderr, `error: dubot diun requires --image and --container (real names from your host)

Example:
  dubot diun --image ghcr.io/example/app:latest --container myproject-app-1

Find a name with: docker ps --format '{{.Names}}\t{{.Image}}'

Do not use nginx unless you actually run an nginx container — UPDATE will fail with container not found.`)
			os.Exit(2)
		}
		cliDiun(image, digest, container)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printCLIHelp()
		os.Exit(2)
	}
}

func printCLIHelp() {
	fmt.Fprintf(os.Stdout, `dubot — DockerUpBot debug commands (run inside the container)

Usage:
  dubot health                 Process health JSON
  dubot telegram               Check Telegram API (getMe, no chat message)
  dubot ping                   Send a test Telegram message to notify targets
  dubot diun --image IMG --container NAME
                               Fire a manual Diun-style webhook (both flags required)
  dubot help                   Show this help

From the host:
  docker compose exec dockerupbot dubot ping
  docker compose exec dockerupbot dubot telegram
  docker compose exec dockerupbot dubot diun --image IMAGE --container CONTAINER_NAME

CONTAINER_NAME must exist (docker ps). Clicking UPDATE will pull + recreate that compose service.
`)
}

func cliGET(path string) {
	url := baseURL() + path
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		failCLI("%v", err)
	}
	if secret := webhookSecret(); secret != "" {
		req.Header.Set("Authorization", secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		failCLI("request failed (%s). Is the server running? %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
	if resp.StatusCode >= 300 {
		os.Exit(1)
	}
}

func cliDiun(image, digest, container string) {
	secret := webhookSecret()
	payload := map[string]any{
		"status":       "update",
		"image":        image,
		"digest":       digest,
		"platform":     "linux/amd64",
		"hostname":     "dubot-manual",
		"diun_version": "manual",
		"metadata": map[string]string{
			"ctn_names": container,
		},
	}
	b, _ := json.Marshal(payload)
	url := baseURL() + "/webhook/diun"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		failCLI("%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		failCLI("webhook request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Printf("status=%d\n", resp.StatusCode)
	if len(body) > 0 {
		fmt.Println(string(body))
	}
	fmt.Printf("triggered image=%s digest=%s container=%s\n", image, digest, container)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		os.Exit(1)
	}
	fmt.Println("ok — check: docker compose logs -f dockerupbot")
}

func webhookSecret() string {
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		secret = os.Getenv("DOCKERUPBOT_WEBHOOK_SECRET")
	}
	return secret
}

func failCLI(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
