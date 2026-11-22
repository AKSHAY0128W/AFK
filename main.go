package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// BotManager dynamically orchestrates active bot workers in goroutines
type BotManager struct {
	mu           sync.Mutex
	workers      map[string]context.CancelFunc
	rootCtx      context.Context
	host         string
	port         int
	transport    string
	gameVersion  string
	authInput    bool
	ctrlCenter   string
}

func NewBotManager(ctx context.Context, host string, port int, transport string, version string, authInput bool, ctrlCenter string) *BotManager {
	return &BotManager{
		workers:     make(map[string]context.CancelFunc),
		rootCtx:     ctx,
		host:        host,
		port:        port,
		transport:   transport,
		gameVersion: version,
		authInput:   authInput,
		ctrlCenter:  strings.TrimRight(ctrlCenter, "/"),
	}
}

// ActiveBotNames returns a sorted/clean list of currently active bot names
func (bm *BotManager) ActiveBotNames() []string {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	var names []string
	for name := range bm.workers {
		names = append(names, name)
	}
	return names
}

// Sync updates the running bot pool to match the desired list exactly
func (bm *BotManager) Sync(desiredNames []string) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	desiredMap := make(map[string]bool)
	for _, n := range desiredNames {
		n = strings.TrimSpace(n)
		if n != "" {
			desiredMap[n] = true
		}
	}

	// 1. Stop any bots that are no longer in the desired list
	for name, cancel := range bm.workers {
		if !desiredMap[name] {
			log.Printf("[BotManager] Stopping removed bot: %s", name)
			cancel()
			delete(bm.workers, name)
		}
	}

	// 2. Start any new bots that are not yet running
	for name := range desiredMap {
		if _, running := bm.workers[name]; !running {
			log.Printf("[BotManager] Launching new dynamic bot worker: %s", name)
			botCtx, botCancel := context.WithCancel(bm.rootCtx)
			bm.workers[name] = botCancel

			go bm.runBotWorker(botCtx, name)
		}
	}
}

// UpdateTarget updates the BDS host/port dynamically and restarts active workers if changed
func (bm *BotManager) UpdateTarget(newHost string, newPort int) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	newHost = strings.TrimSpace(newHost)
	if newHost == "" || newPort <= 0 {
		return
	}

	if bm.host == newHost && bm.port == newPort {
		return
	}

	log.Printf("[BotManager] Target BDS server changed: %s:%d -> %s:%d. Reconnecting running bots...", bm.host, bm.port, newHost, newPort)
	bm.host = newHost
	bm.port = newPort

	// Cancel existing bot sessions so they reconnect to the new host/port
	for name, cancel := range bm.workers {
		log.Printf("[BotManager] Restarting bot %s to connect to new server %s:%d", name, newHost, newPort)
		cancel()
		delete(bm.workers, name)

		botCtx, botCancel := context.WithCancel(bm.rootCtx)
		bm.workers[name] = botCancel
		go bm.runBotWorker(botCtx, name)
	}
}

// StopBot cancels a specific bot by name
func (bm *BotManager) StopBot(name string) bool {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	if cancel, exists := bm.workers[name]; exists {
		log.Printf("[BotManager] Manually stopping bot: %s", name)
		cancel()
		delete(bm.workers, name)
		return true
	}
	return false
}

// StartBot launches a single bot if not already running
func (bm *BotManager) StartBot(name string) bool {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if _, exists := bm.workers[name]; exists {
		return false // already running
	}
	log.Printf("[BotManager] Starting single bot: %s", name)
	botCtx, botCancel := context.WithCancel(bm.rootCtx)
	bm.workers[name] = botCancel
	go bm.runBotWorker(botCtx, name)
	return true
}

func (bm *BotManager) runBotWorker(ctx context.Context, botName string) {
	prefix := fmt.Sprintf("[%s]", botName)
	backoff := 5 * time.Second
	const maxBackoff = 120 * time.Second

	for {
		select {
		case <-ctx.Done():
			log.Printf("%s Bot worker canceled or removed. Disconnecting cleanly.", prefix)
			bm.reportTelemetry(botName, "offline", 0, 0, 0, 0, 0, "")
			return
		default:
		}

		connectedAt := time.Now()
		log.Printf("%s Dialing server (%s:%d) over %s...", prefix, bm.host, bm.port, bm.transport)
		bm.reportTelemetry(botName, "connecting", 0, 0, 0, 0, 0, "")

		err := bm.runBotSession(ctx, botName)
		if err != nil {
			log.Printf("%s Session ended with error: %v", prefix, err)
			bm.reportTelemetry(botName, "reconnecting", 0, 0, 0, 0, 0, err.Error())
		} else {
			log.Printf("%s Session terminated cleanly.", prefix)
			bm.reportTelemetry(botName, "offline", 0, 0, 0, 0, 0, "")
			return
		}

		select {
		case <-ctx.Done():
			return
		default:
		}

		if time.Since(connectedAt) > 30*time.Second {
			backoff = 5 * time.Second
		}

		log.Printf("%s Reconnecting in %v (capped at %v)...", prefix, backoff, maxBackoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (bm *BotManager) runBotSession(ctx context.Context, botName string) error {
	prefix := fmt.Sprintf("[%s]", botName)

	botUUID := uuid.NewSHA1(uuid.NameSpaceDNS, []byte("OfflinePlayer:"+botName)).String()

	dialer := minecraft.Dialer{
		IdentityData: login.IdentityData{
			DisplayName: botName,
			Identity:    botUUID,
		},
		ClientData: login.ClientData{
			GameVersion: bm.gameVersion,
		},
	}

	var address string
	var network string

	switch strings.ToLower(bm.transport) {
	case "nethernet":
		network = "nethernet"
		if strings.HasPrefix(bm.host, "http://") || strings.HasPrefix(bm.host, "https://") {
			address = fmt.Sprintf("%s:%d", bm.host, bm.port)
		} else {
			address = fmt.Sprintf("http://%s:%d", bm.host, bm.port)
		}
	case "raknet":
		network = "raknet"
		address = fmt.Sprintf("%s:%d", bm.host, bm.port)
	default:
		return fmt.Errorf("unsupported transport '%s' (use 'nethernet' or 'raknet')", bm.transport)
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 25*time.Second)
	defer dialCancel()

	log.Printf("%s Connecting to %s [%s] (UUID: %s)...", prefix, address, network, botUUID)
	conn, err := dialer.DialContext(dialCtx, network, address)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer conn.Close()

	// Ensure connection is immediately closed the moment context is canceled
	closeConnOnDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.WritePacket(&packet.Disconnect{
				Message: "Disconnected by AFK Bot Manager",
			})
			_ = conn.Close()
		case <-closeConnOnDone:
		}
	}()
	defer close(closeConnOnDone)

	log.Printf("%s Connected to %s. Handshaking and completing spawn sequence (DoSpawn)...", prefix, conn.RemoteAddr())
	if err := conn.DoSpawn(); err != nil {
		return fmt.Errorf("spawn sequence (DoSpawn) failed: %w", err)
	}

	gameData := conn.GameData()
	log.Printf("%s SPAWN SUCCESS! World: '%s', Dimension: %d, Difficulty: %d",
		prefix, gameData.WorldName, gameData.Dimension, gameData.Difficulty)
	log.Printf("%s Bot Runtime Entity ID: %d, Spawn Coordinates: X=%.2f, Y=%.2f, Z=%.2f",
		prefix, gameData.EntityRuntimeID, gameData.PlayerPosition.X(), gameData.PlayerPosition.Y(), gameData.PlayerPosition.Z())

	// Report successful spawn to Control Center
	bm.reportTelemetry(botName, "online", gameData.PlayerPosition.X(), gameData.PlayerPosition.Y(), gameData.PlayerPosition.Z(), gameData.Dimension, 0, "")

	var posMu sync.RWMutex
	currentPos := gameData.PlayerPosition
	currentPitch := gameData.Pitch
	currentYaw := gameData.Yaw
	botRuntimeID := gameData.EntityRuntimeID

	stopAuthInput := make(chan struct{})
	defer close(stopAuthInput)

	if bm.authInput {
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
					// Telemetry update on teleport or movement
					bm.reportTelemetry(botName, "online", p.Position.X(), p.Position.Y(), p.Position.Z(), gameData.Dimension, 0, "")
				}
			case *packet.Disconnect:
				log.Printf("%s Server sent Disconnect packet: %s", prefix, p.Message)
			}
		}
	}()

	select {
	case <-ctx.Done():
		log.Printf("%s Received cancellation. Disconnecting cleanly...", prefix)
		return nil
	case err := <-readErrChan:
		return fmt.Errorf("read packet loop ended: %w", err)
	}
}

// reportTelemetry sends live telemetry to Control Center
func (bm *BotManager) reportTelemetry(botName string, status string, x, y, z float32, dimension int32, reconnectCount int, errMsg string) {
	if bm.ctrlCenter == "" {
		return
	}

	go func() {
		payload := map[string]interface{}{
			"botName":        botName,
			"status":         status,
			"x":              x,
			"y":              y,
			"z":              z,
			"dimension":      dimension,
			"reconnectCount": reconnectCount,
			"error":          errMsg,
		}

		bodyBytes, _ := json.Marshal(payload)
		client := &http.Client{Timeout: 4 * time.Second}
		endpoint := fmt.Sprintf("%s/api/v1/agent/telemetry", bm.ctrlCenter)

		resp, err := client.Post(endpoint, "application/json", bytes.NewBuffer(bodyBytes))
		if err == nil && resp != nil {
			_ = resp.Body.Close()
		}
	}()
}

// pollControlCenter periodically fetches the authoritative bot list from the Control Center
func (bm *BotManager) pollControlCenter(ctx context.Context) {
	if bm.ctrlCenter == "" {
		return
	}

	fetchAndSync := func() {
		endpoint := fmt.Sprintf("%s/api/v1/agent/bots", bm.ctrlCenter)
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(endpoint)
		if err != nil {
			return
		}
		defer resp.Body.Close()

		var result struct {
			Success bool `json:"success"`
			Server  struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			} `json:"server"`
			Bots []struct {
				Name    string `json:"name"`
				Enabled bool   `json:"enabled"`
			} `json:"bots"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.Success {
			if result.Server.Host != "" && result.Server.Port > 0 {
				bm.UpdateTarget(result.Server.Host, result.Server.Port)
			}
			var desired []string
			for _, b := range result.Bots {
				if b.Enabled {
					desired = append(desired, b.Name)
				}
			}
			log.Printf("[BotManager] Polled Control Center: desired active bots = %v (Target BDS: %s:%d)", desired, bm.host, bm.port)
			bm.Sync(desired)
		}
	}

	// Run immediate sync on start
	fetchAndSync()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fetchAndSync()
		}
	}
}

func main() {
	defaultHost := getEnv("HOST", "ayx.acido.dev")
	defaultPort := 19132
	defaultName := getEnv("BOT_NAME", "")
	defaultTransport := getEnv("TRANSPORT", "nethernet")
	defaultGameVersion := getEnv("GAME_VERSION", "1.26.61")
	defaultAuthInput := getEnvBool("AUTH_INPUT", false)
	defaultCtrlCenter := getEnv("CONTROL_CENTER_URL", "https://our-smp-control-center.onrender.com")

	host := flag.String("host", defaultHost, "BDS server host or IP")
	port := flag.Int("port", defaultPort, "BDS server port")
	name := flag.String("name", defaultName, "Comma-separated bot player display name(s)")
	transport := flag.String("transport", defaultTransport, "Transport network: 'nethernet' or 'raknet'")
	gameVersion := flag.String("game-version", defaultGameVersion, "Game version string reported to server")
	authInput := flag.Bool("auth-input", defaultAuthInput, "Send 20Hz PlayerAuthInput packets to simulate active movement")
	ctrlCenter := flag.String("control-center", defaultCtrlCenter, "OUR SMP Control Center URL for dynamic synchronization")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	var initialBotNames []string
	if *name != "" {
		for _, n := range strings.Split(*name, ",") {
			n = strings.TrimSpace(n)
			if n != "" && n != "IronBot" && n != "IronBot2" {
				initialBotNames = append(initialBotNames, n)
			}
		}
	}

	log.Printf("[System] Starting OUR SMP AFK Bot Service for Minecraft Bedrock")
	log.Printf("[System] Initial Bots: %s", strings.Join(initialBotNames, ", "))
	log.Printf("[System] Target: %s:%d (Transport: %s, GameVersion: %s)", *host, *port, *transport, *gameVersion)
	if *ctrlCenter != "" {
		log.Printf("[System] Control Center Integration: ACTIVE (%s)", *ctrlCenter)
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	botManager := NewBotManager(rootCtx, *host, *port, *transport, *gameVersion, *authInput, *ctrlCenter)

	// Start initial configured bots
	botManager.Sync(initialBotNames)

	// Background polling for changes from Control Center
	if *ctrlCenter != "" {
		go botManager.pollControlCenter(rootCtx)
	}

	// HTTP Web Server on Render PORT
	webPort := os.Getenv("PORT")
	if webPort == "" {
		webPort = "8080"
	}

	mux := http.NewServeMux()

	// 1. Health check & Render root
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		active := botManager.ActiveBotNames()
		_, _ = fmt.Fprintf(w, "OUR SMP AFK Bot Worker is ACTIVE!\nRunning bots (%d): %s\nTarget: %s:%d\nTimestamp: %s\n",
			len(active), strings.Join(active, ", "), *host, *port, time.Now().UTC().Format(time.RFC3339))
	})

	// 2. GET /api/bots - Returns active bot list
	mux.HandleFunc("/api/bots", func(w http.ResponseWriter, r *http.Request) {
		active := botManager.ActiveBotNames()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"bots":    active,
			"count":   len(active),
		})
	})

	// 3. POST /api/bots/sync - Receives desired list from Control Center and synchronizes immediately
	mux.HandleFunc("/api/bots/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Bots []string `json:"bots"`
			Host string   `json:"host,omitempty"`
			Port int      `json:"port,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}

		if req.Host != "" && req.Port > 0 {
			botManager.UpdateTarget(req.Host, req.Port)
		}

		log.Printf("[HTTP] Received dynamic bot sync request with %d bots (Target: %s:%d): %v", len(req.Bots), botManager.host, botManager.port, req.Bots)
		botManager.Sync(req.Bots)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"active":  botManager.ActiveBotNames(),
		})
	})

	// 4. POST /api/bots/add - Dynamically add a single bot
	mux.HandleFunc("/api/bots/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "Valid 'name' is required", http.StatusBadRequest)
			return
		}

		started := botManager.StartBot(req.Name)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": started,
			"name":    req.Name,
			"active":  botManager.ActiveBotNames(),
		})
	})

	// 5. POST /api/bots/remove - Dynamically stop a single bot
	mux.HandleFunc("/api/bots/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "Valid 'name' is required", http.StatusBadRequest)
			return
		}

		stopped := botManager.StopBot(req.Name)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": stopped,
			"name":    req.Name,
			"active":  botManager.ActiveBotNames(),
		})
	})

	go func() {
		log.Printf("[System] Health check & Management API server listening on :%s", webPort)
		_ = http.ListenAndServe(":"+webPort, mux)
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	log.Printf("[System] Shutdown signal received. Disconnecting all bots cleanly...")
	rootCancel()
	log.Printf("[System] All bots stopped safely. Exiting.")
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	return strings.ToLower(val) == "true" || val == "1"
}
