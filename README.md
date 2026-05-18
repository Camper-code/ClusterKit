# ClusterKit

Мінімальна UI-апка для локального LLM-кластера на `llama.cpp` RPC.

## UX

Користувач відкриває застосунок, бачить локальний IP, вибирає роль:

- **Worker** — запускає `rpc-server` на цьому ноуті.
- **Coordinator** — запускає `llama-server` і підключає workers.

UI доступний локально на `http://127.0.0.1:8765`, але сам застосунок відкриває його автоматично.

## Пакування

### macOS

```bash
./scripts/package-macos.sh
```

Результат:

```text
dist/ClusterKit.app
dist/ClusterKit-macos-arm64.zip
```

### Windows

На Windows PowerShell:

```powershell
.\scripts\package-windows.ps1
```

Результат:

```text
dist\ClusterKit-windows-amd64.zip
```

Всередині: `ClusterKit.exe`, `start.bat`, `install.bat`.

## Важливо

Це UI-shell. Для реального inference на машині має бути зібраний `llama.cpp` з `GGML_RPC=ON`:

- macOS: Metal + RPC
- Windows: Vulkan/CPU + RPC

У полі `llama.cpp папка` вказати шлях до папки, де є `build/bin/rpc-server` і `build/bin/llama-server`.

## Наступні поліпшення

- автозавантаження prebuilt llama.cpp release
- авто-discovery workers у LAN
- firewall helper
- вибір GGUF через native file picker
- красивий native window через WebView/Tauri/Wails, якщо треба без браузера
