package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func main() {
	host := flag.String("host", "ayx.acido.dev", "BDS server host or IP")
	port := flag.Int("port", 19132, "BDS server port")
	name := flag.String("name", "IronBot", "Bot player display name")
	transport := flag.String("transport", "nethernet", "Transport network: 'nethernet' or 'raknet'")
	gameVersion := flag.String("game-version", "1.26.61", "Game version string reported to server")
	authInput := flag.Bool("auth-input", false, "Send 20Hz PlayerAuthInput packets to simulate active movement")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Printf("[IronBot] Starting AFK Player Bot for Minecraft Bedrock")
	log.Printf("[IronBot] Target: %s:%d (Transport: %s, Offline Account: %s)", *host, *port, *transport, *name)
	if *authInput {
		log.Printf("[IronBot] PlayerAuthInput is ENABLED (transmitting 20Hz active player input)")
	} else {
		log.Printf("[IronBot] PlayerAuthInput is DISABLED (idle client packet loop)")
	}

	// Start lightweight HTTP health-check server for cloud hosting & keep-alive pings
	webPort := os.Getenv("PORT")
	if webPort == "" {
		webPort = "8080"
	}
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "IronBot is active and running!\n")
		})
		log.Printf("[IronBot] Health check web server listening on port :%s", webPort)
		_ = http.ListenAndServe(":"+webPort, mux)
	}()

	// Capture interrupt signals for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	backoff := 5 * time.Second
	const maxBackoff = 120 * time.Second // 2 minute cap

	for {
		connectedAt := time.Now()
		log.Printf("[IronBot] Dialing server (%s) over %s...", fmt.Sprintf("%s:%d", *host, *port), *transport)

		err := runBotSession(*host, *port, *transport, *name, *gameVersion, *authInput, sigChan)
		if err != nil {
			log.Printf("[IronBot] Session ended with error: %v", err)
		} else {
			log.Printf("[IronBot] Session terminated normally or shutdown signal received")
			return
		}

		// If connected for at least 30 seconds before disconnecting, reset backoff
		if time.Since(connectedAt) > 30*time.Second {
			backoff = 5 * time.Second
		}

		log.Printf("[IronBot] Reconnecting in %v (capped at %v)...", backoff, maxBackoff)
		select {
		case <-sigChan:
			log.Printf("[IronBot] Shutdown signal received during backoff. Exiting.")
			return
		case <-time.After(backoff):
		}

		// Exponential backoff
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runBotSession handles a single connection lifecycle.
func runBotSession(host string, port int, transport string, botName string, gameVersion string, enableAuthInput bool, sigChan <-chan os.Signal) error {
	dialer := minecraft.Dialer{
		IdentityData: login.IdentityData{
			DisplayName: botName,
		},
		ClientData: login.ClientData{
			GameVersion: gameVersion,
		},
	}

	var address string
	var network string

	switch strings.ToLower(transport) {
	case "nethernet":
		network = "nethernet"
		// NetherNet requires an HTTP signaling address URL with port
		if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
			address = fmt.Sprintf("%s:%d", host, port)
		} else {
			address = fmt.Sprintf("http://%s:%d", host, port)
		}
	case "raknet":
		network = "raknet"
		address = fmt.Sprintf("%s:%d", host, port)
	default:
		return fmt.Errorf("unsupported transport '%s' (use 'nethernet' or 'raknet')", transport)
	}

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer dialCancel()

	log.Printf("[IronBot] Connecting to %s [%s]...", address, network)
	conn, err := dialer.DialContext(dialCtx, network, address)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer conn.Close()

	log.Printf("[IronBot] Connected to %s. Handshaking and completing spawn sequence (DoSpawn)...", conn.RemoteAddr())
	if err := conn.DoSpawn(); err != nil {
		return fmt.Errorf("spawn sequence (DoSpawn) failed: %w", err)
	}

	gameData := conn.GameData()
	log.Printf("[IronBot] SPAWN SUCCESS! World: '%s', Dimension: %d, Difficulty: %d",
		gameData.WorldName, gameData.Dimension, gameData.Difficulty)
	log.Printf("[IronBot] Bot Runtime Entity ID: %d, Spawn Coordinates: X=%.2f, Y=%.2f, Z=%.2f",
		gameData.EntityRuntimeID, gameData.PlayerPosition.X(), gameData.PlayerPosition.Y(), gameData.PlayerPosition.Z())
	log.Printf("[IronBot] Bot is now active in server player list. Waiting for incoming packets / commands...")

	var posMu sync.RWMutex
	currentPos := gameData.PlayerPosition
	currentPitch := gameData.Pitch
	currentYaw := gameData.Yaw
	botRuntimeID := gameData.EntityRuntimeID

	stopAuthInput := make(chan struct{})
	defer close(stopAuthInput)

	// Optional 20Hz PlayerAuthInput loop to simulate player inputs
	if enableAuthInput {
		go func() {
			ticker := time.NewTicker(50 * time.Millisecond) // 20 ticks / second
			defer ticker.Stop()
			var tickCount uint64

			for {
				select {
				case <-stopAuthInput:
					return
				case <-ticker.C:
					tickCount++
					posMu.RLock()
					p := currentPos
					pi := currentPitch
					y := currentYaw
					posMu.RUnlock()

					input := &packet.PlayerAuthInput{
						Pitch:            pi,
						Yaw:              y,
						HeadYaw:          y,
						Position:         p,
						Tick:             tickCount,
						InputMode:        packet.InputModeTouch,
						PlayMode:         packet.PlayModeNormal,
						InteractionModel: packet.InteractionModelClassic,
					}
					if err := conn.WritePacket(input); err != nil {
						// Write error means connection dropped
						return
					}
				}
			}
		}()
	}

	// Read packets in a loop to keep the connection healthy and track updates
	readErrChan := make(chan error, 1)
	go func() {
		for {
			pk, err := conn.ReadPacket()
			if err != nil {
				readErrChan <- err
				return
			}

			switch p := pk.(type) {
			case *packet.MovePlayer:
				if p.EntityRuntimeID == botRuntimeID {
					posMu.Lock()
					currentPos = p.Position
					currentPitch = p.Pitch
					currentYaw = p.Yaw
					posMu.Unlock()
					log.Printf("[IronBot] Position updated by server (/tp or move): X=%.2f, Y=%.2f, Z=%.2f (Yaw=%.1f)",
						p.Position.X(), p.Position.Y(), p.Position.Z(), p.Yaw)
				}
			case *packet.Disconnect:
				log.Printf("[IronBot] Server sent Disconnect packet: %s", p.Message)
			case *packet.Text:
				if p.TextType == packet.TextTypeChat || p.TextType == packet.TextTypeTranslation {
					// Optionally log chat messages if relevant
					// log.Printf("[Chat] %s: %s", p.SourceName, p.Message)
				}
			}
		}
	}()

	select {
	case sig := <-sigChan:
		log.Printf("[IronBot] Received signal %v. Disconnecting cleanly...", sig)
		return nil
	case err := <-readErrChan:
		return fmt.Errorf("read packet loop ended: %w", err)
	}
}
