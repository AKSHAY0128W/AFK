# IronBot - 24/7 Minecraft Bedrock AFK Player Bot

A custom, native Go client for Minecraft Bedrock Dedicated Server (BDS) built with `github.com/sandertv/gophertunnel`. It connects as a regular player named `IronBot` to keep iron farms and village chunks loaded 24/7 without requiring the game to be open.

---

## 1. Version Compatibility Verification: 1.26.61.1

### Supported Protocol Check
- **`gophertunnel` master / v1.63.0:**
  - `minecraft/protocol/info.go`: `CurrentProtocol = 2225`, `CurrentVersion = "1.26.60"`.
  - `go.mod`: requires `go 1.25.0` (compatible with Go 1.24+).
- **BDS Server Telemetry:**
  We verified your server endpoint directly at `http://ayx.acido.dev:19132/v1/join`:
  ```json
  {"name":"our smp 247","protocol":2225,"version":"1.26.61","level":"CherryVale SMP","players":0,"maxPlayers":10,"gameType":0}
  ```
- **Conclusion:** BDS `1.26.61.1` is a hotfix release within the `1.26.60` cycle that shares the **exact same network protocol ID (2225)**.
- In `main.go`, we explicitly configure `ClientData.GameVersion = "1.26.61"` so BDS sees a 100% version match.

---

## 2. What `transport=nethernet` Means (Crucial Finding)

Your `server.properties` specifies `transport=nethernet`.
- **RakNet (Legacy):** Pure UDP on port 19132. We tested a RakNet unconnected ping against your server, and it **timed out**.
- **NetherNet (Modern Bedrock):** Uses **HTTP/WebRTC** signaling on port 19132. We tested HTTP `GET http://ayx.acido.dev:19132/v1/join`, and it returned **HTTP 200 OK** with your server's live JSON status.
- **Why this matters:** If you dial over `"raknet"`, your connection will fail with a timeout! `gophertunnel` natively supports NetherNet via `github.com/df-mc/go-nethernet`.
- **Address format:** NetherNet requires an HTTP URL: `http://ayx.acido.dev:19132`.

---

## 3. Quick Start & Testing

> [!IMPORTANT]
> Always test on a backup/staging server or world copy first before deploying to your main world.

### Prerequisites (Install Go)
If Go is not installed on your machine:
```powershell
winget install GoLang.Go
```
*Restart your terminal after installation so `go` is in your PATH.*

### Running the Bot
From this directory (`c:\Users\HP\Downloads\AFK`):
```powershell
# 1. Download module dependencies
go mod tidy

# 2. Run the bot (Default mode: NetherNet, idle packet loop)
go run main.go
```

### Running with PlayerAuthInput (Active Simulation Mode)
If your iron farm requires continuous player input packets:
```powershell
go run main.go -auth-input=true
```

---

## 4. Step-by-Step Test Procedure

1. **Verify Join:**
   - Run `go run main.go`.
   - The bot logs:
     ```text
     [IronBot] Connected to http://ayx.acido.dev:19132. Handshaking and completing spawn sequence (DoSpawn)...
     [IronBot] SPAWN SUCCESS! World: 'CherryVale SMP', Dimension: 0, Difficulty: 3
     [IronBot] Bot Runtime Entity ID: 12345, Spawn Coordinates: X=..., Y=..., Z=...
     ```
   - In your Acido Pterodactyl panel console, run:
     ```text
     list
     ```
   - Confirm `IronBot` appears in the list of online players.

2. **Teleport Bot to the Farm:**
   - In your panel console, run:
     ```text
     tp IronBot 40 108 -13
     ```
   - The bot console will immediately log:
     ```text
     [IronBot] Position updated by server (/tp or move): X=40.00, Y=108.00, Z=-13.00
     ```
   - BDS persists player coordinates on offline servers in `players/<UUID>.dat`. On reconnects, the bot automatically spawns back at these coordinates.

3. **Verify Farm Production:**
   - All human players log off or move > 128 blocks away from the iron farm.
   - Wait 10-15 minutes.
   - Check the collection chest: iron ingots and poppies should accumulate.
   - *If no golems spawn:* Restart the bot with `-auth-input=true` to transmit active 20Hz movement input.

4. **Verify Reconnect Resilience:**
   - In the Pterodactyl panel, restart the BDS server.
   - The bot will detect the disconnect:
     ```text
     [IronBot] Session ended with error: read packet loop ended: EOF
     [IronBot] Reconnecting in 5s (capped at 2m0s)...
     ```
   - Once the BDS server finishes booting, the bot reconnects automatically and logs `SPAWN SUCCESS!`.

---

## 5. 24/7 Isolated Deployment

### Option A: Docker (Recommended)
```bash
docker compose up -d
```
View logs:
```bash
docker compose logs -f
```

### Option B: Linux systemd
1. Copy the compiled binary:
   ```bash
   go build -o ironbot main.go
   sudo mkdir -p /opt/ironbot
   sudo cp ironbot /opt/ironbot/
   ```
2. Install the systemd service:
   ```bash
   sudo cp ironbot.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now ironbot
   ```
3. Monitor status:
   ```bash
   sudo journalctl -u ironbot -f
   ```
