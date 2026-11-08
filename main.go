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

	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func main() {
	host := flag.String("host", "ayx.acido.dev", "BDS server host or IP")
	port := flag.Int("port", 19132, "BDS server port")
	name := flag.String("name", "IronBot,IronBot2", "Comma-separated bot player display name(s)")
	transport := flag.String("transport", "nethernet", "Transport network: 'nethernet' or 'raknet'")
	gameVersion := flag.String("game-version", "1.26.61", "Game version string reported to server")
	authInput := flag.Bool("auth-input", false, "Send 20Hz PlayerAuthInput packets to simulate active movement")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	var botNames []string
	for _, n := range strings.Split(*name, ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			botNames = append(botNames, n)
		}
	}
	if len(botNames) == 0 {
		botNames = []string{"IronBot"}
	}

	log.Printf("[System] Starting AFK Player Bot(s) for Minecraft Bedrock: %s", strings.Join(botNames, ", "))
	log.Printf("[System] Target: %s:%d (Transport: %s, GameVersion: %s)", *host, *port, *transport, *gameVersion)
	if *authInput {
		log.Printf("[System] PlayerAuthInput is ENABLED (transmitting 20Hz active player input)")
	} else {
		log.Printf("[System] PlayerAuthInput is DISABLED (idle client packet loop)")
	}

	webPort := os.Getenv("PORT")
	if webPort == "" {
		webPort = "8080"
	}
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "AFK Bots are active: %s\n", strings.Join(botNames, ", "))
		})
		log.Printf("[System] Health check web server listening on port :%s", webPort)
		_ = http.ListenAndServe(":"+webPort, mux)
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	var wg sync.WaitGroup
	for i, bName := range botNames {
		wg.Add(1)
		delay := time.Duration(i*3) * time.Second
		go func(botName string, startDelay time.Duration) {
			defer wg.Done()
			if startDelay > 0 {
				select {
				case <-sigChan:
					return
				case <-time.After(startDelay):
				}
			}
			runBotLoop(*host, *port, *transport, botName, *gameVersion, *authInput, sigChan)
		}(bName, delay)
	}

	<-sigChan
	log.Printf("[System] Shutdown signal received. Waiting for bots to disconnect cleanly...")
	wg.Wait()
	log.Printf("[System] All bots stopped. Exiting.")
}

func runBotLoop(host string, port int, transport string, botName string, gameVersion string, enableAuthInput bool, sigChan <-chan os.Signal) {
	prefix := fmt.Sprintf("[%s]", botName)
	backoff := 5 * time.Second
	const maxBackoff = 120 * time.Second

	for {
		connectedAt := time.Now()
		log.Printf("%s Dialing server (%s:%d) over %s...", prefix, host, port, transport)

		err := runBotSession(host, port, transport, botName, gameVersion, enableAuthInput, sigChan)
		if err != nil {
			log.Printf("%s Session ended with error: %v", prefix, err)
		} else {
			log.Printf("%s Session terminated normally or shutdown signal received", prefix)
			return
		}

		select {
		case <-sigChan:
			log.Printf("%s Stopping bot loop on shutdown signal", prefix)
			return
		default:
		}

		if time.Since(connectedAt) > 30*time.Second {
			backoff = 5 * time.Second
		}

		log.Printf("%s Reconnecting in %v (capped at %v)...", prefix, backoff, maxBackoff)
		select {
		case <-sigChan:
			log.Printf("%s Shutdown signal received during backoff. Exiting.", prefix)
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func runBotSession(host string, port int, transport string, botName string, gameVersion string, enableAuthInput bool, sigChan <-chan os.Signal) error {
	prefix := fmt.Sprintf("[%s]", botName)

	// Fixed deterministic UUID per bot name so BDS always maps player files consistently
	botUUID := uuid.NewSHA1(uuid.NameSpaceDNS, []byte("OfflinePlayer:"+botName)).String()

	dialer := minecraft.Dialer{
		IdentityData: login.IdentityData{
			DisplayName: botName,
			Identity:    botUUID,
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

	log.Printf("%s Connecting to %s [%s] (UUID: %s)...", prefix, address, network, botUUID)
	conn, err := dialer.DialContext(dialCtx, network, address)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer conn.Close()

	log.Printf("%s Connected to %s. Handshaking and completing spawn sequence (DoSpawn)...", prefix, conn.RemoteAddr())
	if err := conn.DoSpawn(); err != nil {
		return fmt.Errorf("spawn sequence (DoSpawn) failed: %w", err)
	}

	gameData := conn.GameData()
	log.Printf("%s SPAWN SUCCESS! World: '%s', Dimension: %d, Difficulty: %d",
		prefix, gameData.WorldName, gameData.Dimension, gameData.Difficulty)
	log.Printf("%s Bot Runtime Entity ID: %d, Spawn Coordinates: X=%.2f, Y=%.2f, Z=%.2f",
		prefix, gameData.EntityRuntimeID, gameData.PlayerPosition.X(), gameData.PlayerPosition.Y(), gameData.PlayerPosition.Z())

	var posMu sync.RWMutex
	currentPos := gameData.PlayerPosition
	currentPitch := gameData.Pitch
	currentYaw := gameData.Yaw
	botRuntimeID := gameData.EntityRuntimeID

	stopAuthInput := make(chan struct{})
	defer close(stopAuthInput)

	if enableAuthInput {
		go func() {
			ticker := time.NewTicker(50 * time.Millisecond)
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
						return
					}
				}
			}
		}()
	}

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
					log.Printf("%s Position updated by server (/tp or move): X=%.2f, Y=%.2f, Z=%.2f (Yaw=%.1f)",
						prefix, p.Position.X(), p.Position.Y(), p.Position.Z(), p.Yaw)
				}
			case *packet.Disconnect:
				log.Printf("%s Server sent Disconnect packet: %s", prefix, p.Message)
			}
		}
	}()

	select {
	case sig := <-sigChan:
		log.Printf("%s Received signal %v. Disconnecting cleanly...", prefix, sig)
		return nil
	case err := <-readErrChan:
		return fmt.Errorf("read packet loop ended: %w", err)
	}
}
