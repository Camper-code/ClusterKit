package main

import (
	"archive/zip"
	"bufio"
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

//go:embed ui/*
var uiFS embed.FS

type Config struct {
	Role              string   `json:"role"`
	RoleExplicit      bool     `json:"roleExplicit"`
	ModelPath         string   `json:"modelPath"`
	ModelsDir         string   `json:"modelsDir"`
	APIPort           int      `json:"apiPort"`
	RPCPort           int      `json:"rpcPort"`
	Workers           []Worker `json:"workers"`
	LlamaDir          string   `json:"llamaDir"`
	Context           int      `json:"context"`
	GPULayers         int      `json:"gpuLayers"`
	Threads           int      `json:"threads"`
	Parallel          int      `json:"parallel"`
	CacheRAM          int      `json:"cacheRam"`
	ChatTimeout       int      `json:"chatTimeoutSec"`
	ChatMaxTokens     int      `json:"chatMaxTokens"`
	ChatNoTokenLimit  bool     `json:"chatNoTokenLimit"`
	HideThinking      bool     `json:"hideThinking,omitempty"`
	Batch             int      `json:"batch"`
	UBatch            int      `json:"uBatch"`
	TensorSplit       string   `json:"tensorSplit"`
	SplitMode         string   `json:"splitMode"`
	ComputeMode       string   `json:"computeMode"`
	MemoryMode        string   `json:"memoryMode"`
	CoordinatorLocal  bool     `json:"coordinatorLocal"`
	CoordinatorLayers int      `json:"coordinatorLayers,omitempty"`
	WorkerPolicy      string   `json:"workerPolicy"`
	WeightedMode      bool     `json:"weightedMode"`
}

type Worker struct {
	Name        string  `json:"name"`
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	Disabled    bool    `json:"disabled,omitempty"`
	Layers      int     `json:"layers,omitempty"`
	SplitWeight float64 `json:"splitWeight,omitempty"`
	AppPort     int     `json:"appPort,omitempty"`
	OK          bool    `json:"ok,omitempty"`
	Status      string  `json:"status,omitempty"`
	SeenMs      int64   `json:"seenMs,omitempty"`
	OS          string  `json:"os,omitempty"`
	Arch        string  `json:"arch,omitempty"`
	RAMBytes    uint64  `json:"ramBytes,omitempty"`
	VRAMBytes   uint64  `json:"vramBytes,omitempty"`
	Backend     string  `json:"backend,omitempty"`
	Threads     int     `json:"threads,omitempty"`
	CrashCount  int     `json:"crashCount,omitempty"`
	Stability   float64 `json:"stability,omitempty"`
	RSSBytes    uint64  `json:"rssBytes,omitempty"`
	LoadPct     float64 `json:"loadPct,omitempty"`
}

type HardwareInfo struct {
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPUCount  int    `json:"cpuCount"`
	RAMBytes  uint64 `json:"ramBytes"`
	VRAMBytes uint64 `json:"vramBytes,omitempty"`
	Backend   string `json:"backend,omitempty"`
}

var hardwareInfoCache struct {
	sync.Mutex
	value HardwareInfo
	at    time.Time
}

type DiscoveryPeer struct {
	Name       string  `json:"name"`
	Host       string  `json:"host"`
	Port       int     `json:"port"`
	AppPort    int     `json:"appPort,omitempty"`
	Role       string  `json:"role"`
	OS         string  `json:"os"`
	Arch       string  `json:"arch"`
	RAM        uint64  `json:"ramBytes"`
	VRAMBytes  uint64  `json:"vramBytes,omitempty"`
	Backend    string  `json:"backend,omitempty"`
	Threads    int     `json:"threads"`
	CrashCount int     `json:"crashCount,omitempty"`
	Stability  float64 `json:"stability,omitempty"`
	RSSBytes   uint64  `json:"rssBytes,omitempty"`
	LoadPct    float64 `json:"loadPct,omitempty"`
}

type rememberedPeer struct {
	Peer DiscoveryPeer
	Seen time.Time
}

type AppState struct {
	mu                  sync.Mutex
	config              Config
	workerCmd           *exec.Cmd
	serverCmd           *exec.Cmd
	workerStarting      bool
	serverStarting      bool
	workerManualStopped bool
	logs                []string
	discovered          map[string]rememberedPeer
	serverLoad          string
	serverContext       int
	serverRPC           string
	workerStatusCache   []Worker
	workerStatusAt      time.Time
	loadStarted         time.Time
	loadReady           time.Time
	workerCrashCount    int
	workerLastRestart   time.Time
	serverFallbackTried bool
	serverCrashCount    int
	serverLastCrash     time.Time
	appPort             int
	workerStarted       time.Time
	lastSelfHeal        time.Time
	lastCoordinatorHeal time.Time
	download            DownloadStatus
}

type DownloadStatus struct {
	Active     bool   `json:"active"`
	Repo       string `json:"repo,omitempty"`
	File       string `json:"file,omitempty"`
	Path       string `json:"path,omitempty"`
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total,omitempty"`
	Percent    int    `json:"percent,omitempty"`
	SpeedBps   int64  `json:"speedBps,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	StartedMs  int64  `json:"startedMs,omitempty"`
	UpdatedMs  int64  `json:"updatedMs,omitempty"`
}

func main() {
	webMode := flag.Bool("web", false, "open browser web UI")
	noBrowser := flag.Bool("no-browser", false, "do not open browser automatically")
	flag.Parse()

	state := &AppState{config: defaultConfig(), discovered: map[string]rememberedPeer{}}
	_ = os.MkdirAll(appDir(), 0755)
	if cfg, err := loadConfig(); err == nil {
		cfg = resetStartupRole(cfg)
		state.config = cfg
		_ = saveConfig(cfg)
	}
	if *webMode {
		go func() {
			time.Sleep(1200 * time.Millisecond)
			state.autostartWorker()
		}()
	}
	mux := http.NewServeMux()
	ui, _ := fs.Sub(uiFS, "ui")
	mux.Handle("/", http.FileServer(http.FS(ui)))
	mux.HandleFunc("/api/status", state.handleStatus)
	mux.HandleFunc("/api/config", state.handleConfig)
	mux.HandleFunc("/api/save", state.handleSave)
	mux.HandleFunc("/api/start-worker", state.handleStartWorker)
	mux.HandleFunc("/api/stop-worker", state.handleStopWorker)
	mux.HandleFunc("/api/start-coordinator", state.handleStartCoordinator)
	mux.HandleFunc("/api/stop-coordinator", state.handleStopCoordinator)
	mux.HandleFunc("/api/check-workers", state.handleCheckWorkers)
	mux.HandleFunc("/api/discover", state.handleDiscover)
	mux.HandleFunc("/api/optimize", state.handleOptimize)
	mux.HandleFunc("/api/open", state.handleOpen)
	mux.HandleFunc("/api/install", state.handleInstall)
	mux.HandleFunc("/api/models/search", state.handleModelSearch)
	mux.HandleFunc("/api/models/files", state.handleModelFiles)
	mux.HandleFunc("/api/models/local", state.handleLocalModels)
	mux.HandleFunc("/api/models/download", state.handleModelDownload)
	mux.HandleFunc("/api/models/select", state.handleModelSelect)
	mux.HandleFunc("/api/models/delete", state.handleModelDelete)
	mux.HandleFunc("/api/models/cache-clear", state.handleModelCacheClear)
	mux.HandleFunc("/api/chat", state.handleChat)
	mux.HandleFunc("/api/chat-stream", state.handleChatStream)
	// OpenAI-compatible API served by ClusterKit itself. This keeps clients on
	// the stable ClusterKit app port while proxying generation to the active
	// llama.cpp coordinator API port.
	mux.HandleFunc("/health", state.handleOpenAIHealth)
	mux.HandleFunc("/v1", state.handleOpenAIRoot)
	mux.HandleFunc("/v1/health", state.handleOpenAIHealth)
	mux.HandleFunc("/v1/models", state.handleOpenAIModels)
	mux.HandleFunc("/v1/chat/completions", state.handleOpenAIChatCompletions)
	mux.HandleFunc("/v1/completions", state.handleOpenAICompletions)
	// Compatibility aliases for clients that expect base_url to be the server
	// root instead of /v1. This avoids confusing 404s when a client appends its
	// own /v1 inconsistently or is configured with http://host:port.
	mux.HandleFunc("/models", state.handleOpenAIModels)
	mux.HandleFunc("/chat/completions", state.handleOpenAIChatCompletions)
	mux.HandleFunc("/completions", state.handleOpenAICompletions)

	listener, addr, err := localListener(8765)
	if err != nil {
		log.Fatal(err)
	}
	if _, portText, splitErr := net.SplitHostPort(listener.Addr().String()); splitErr == nil {
		state.appPort, _ = strconv.Atoi(portText)
	}
	if *webMode && !*noBrowser {
		go func() {
			time.Sleep(400 * time.Millisecond)
			openBrowser("http://" + addr)
		}()
	}

	go state.discoveryResponder()
	go state.discoveryAnnouncer()
	appendAppLog(fmt.Sprintf("ClusterKit UI: http://%s", addr))
	if !*webMode {
		go func() {
			if err := http.Serve(listener, mux); err != nil {
				appendAppLog("http server error: " + err.Error())
			}
		}()
		runTerminalUI(state, addr)
		return
	}
	if err := http.Serve(listener, mux); err != nil {
		log.Fatal(err)
	}
}

type tuiAction struct {
	Key   string
	Label string
	Run   func()
}

type tuiTick time.Time

type tuiSnapshotMsg map[string]any

type tuiBusyDone struct {
	Label string
	Snap  map[string]any
	Err   error
}

type tuiLocalModel struct {
	Name       string
	Path       string
	Size       int64
	Selected   bool
	MaxContext int
	Aux        bool
}

type tuiHFSearchMsg struct {
	Models []HFModel
	Err    error
}

type tuiHFFilesMsg struct {
	Repo  string
	Files []HFSibling
	Err   error
}

type tuiChatMsg struct {
	Reply  string
	Tokens int
	Ms     int64
	Err    error
}

type tuiChatStreamMsg struct {
	Content string
	Thought string
	Tokens  int
	Ms      int64
	Done    bool
	Err     error
}

type chatLine struct {
	Role   string
	Text   string
	Tokens int
	Ms     int64
	At     time.Time
}

type tuiModel struct {
	state            *AppState
	addr             string
	actions          []tuiAction
	selected         int
	width            int
	height           int
	styles           tuiStyles
	snap             map[string]any
	rolePick         bool
	roleSel          int
	workerDevicePick bool
	workerDeviceSel  int
	busy             bool
	busyText         string
	screen           string
	localSel         int
	hfQuery          string
	hfModels         []HFModel
	hfSel            int
	hfOffset         int
	hfRepo           string
	hfFiles          []HFSibling
	fileSel          int
	fileOffset       int
	browser          string
	localOffset      int
	hfSearchIn       textinput.Model
	hfSearchFocus    bool
	settingSel       int
	workerLayerSel   int
	chatIn           textarea.Model
	chat             []chatLine
	chatUsed         int
	chatStream       <-chan tuiChatStreamMsg
}

type tuiStyles struct {
	base     lipgloss.Style
	box      lipgloss.Style
	title    lipgloss.Style
	muted    lipgloss.Style
	selected lipgloss.Style
	ok       lipgloss.Style
	warn     lipgloss.Style
	bad      lipgloss.Style
	accent   lipgloss.Color
}

func newTUIStyles() tuiStyles {
	accent := lipgloss.Color("208")
	border := lipgloss.RoundedBorder()
	if asciiTUI() {
		border = lipgloss.Border{
			Top:         "-",
			Bottom:      "-",
			Left:        "|",
			Right:       "|",
			TopLeft:     "+",
			TopRight:    "+",
			BottomLeft:  "+",
			BottomRight: "+",
		}
	}
	return tuiStyles{
		base:     lipgloss.NewStyle().Foreground(lipgloss.Color("223")),
		box:      lipgloss.NewStyle().Border(border).BorderForeground(accent).Padding(0, 1),
		title:    lipgloss.NewStyle().Foreground(accent).Bold(true),
		muted:    lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		selected: lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(accent).Bold(true).Padding(0, 1),
		ok:       lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
		warn:     lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		bad:      lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
		accent:   accent,
	}
}

func runTerminalUI(s *AppState, addr string) {
	actions := []tuiAction{
		{Key: "i", Label: "Install / repair", Run: func() { go s.installDeps(false) }},
		{Key: "w", Label: "Start / stop worker", Run: func() {
			s.mu.Lock()
			running := s.workerCmd != nil
			cfg := s.config
			s.mu.Unlock()
			if running {
				s.stopWorkerManual()
			} else if err := s.startWorkerProcess(cfg); err != nil {
				s.addLog("worker start failed: %v", err)
			}
		}},
		{Key: "c", Label: "Start / stop coordinator", Run: func() {
			s.mu.Lock()
			running := s.serverCmd != nil
			cfg := s.config
			s.mu.Unlock()
			if running {
				s.stop("server")
				return
			}
			cfg.Workers = s.classifyWorkers(append([]Worker(nil), cfg.Workers...), 650*time.Millisecond)
			s.mu.Lock()
			s.config = cfg
			s.mu.Unlock()
			_ = saveConfig(cfg)
			if err := s.startCoordinatorProcess(cfg, true); err != nil {
				s.addLog("coordinator start failed: %v", err)
			}
		}},
		{Key: "d", Label: "Discover workers", Run: func() { tuiDiscover(s) }},
		{Key: "a", Label: "API endpoints", Run: func() {}},
		{Key: "l", Label: "Worker layers", Run: func() {}},
		{Key: "m", Label: "Select local model", Run: func() {}},
		{Key: "b", Label: "Browse / download models", Run: func() {}},
		{Key: "s", Label: "Launch settings", Run: func() {}},
		{Key: "t", Label: "Chat", Run: func() {}},
		{Key: "q", Label: "Quit", Run: func() {}},
	}
	input := textarea.New()
	input.Placeholder = terminalText("Ask local model…")
	input.CharLimit = 4000
	input.SetWidth(90)
	input.SetHeight(2)
	search := textinput.New()
	search.Placeholder = terminalText("Search Hugging Face models…")
	search.CharLimit = 200
	search.Width = 80
	search.SetValue("qwen gguf")
	// Always ask on TUI startup. Old configs can remember a role, but startup
	// should still make the user choose what this machine is doing now.
	model := tuiModel{state: s, addr: addr, actions: actions, width: 100, height: 30, styles: newTUIStyles(), snap: s.fastSnapshot(), rolePick: true, chatIn: input, hfSearchIn: search, hfQuery: "qwen gguf"}
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		appendAppLog(fmt.Sprintf("terminal UI error: %v", err))
	}
}

func (m tuiModel) Init() tea.Cmd { return tea.Batch(tuiTickCmd(), m.snapshotCmd()) }

func tuiTickCmd() tea.Cmd {
	return tea.Tick(1500*time.Millisecond, func(t time.Time) tea.Msg { return tuiTick(t) })
}

func (m tuiModel) snapshotCmd() tea.Cmd {
	return func() tea.Msg { return tuiSnapshotMsg(m.state.fastSnapshot()) }
}

func (m tuiModel) hfSearchCmd(query string) tea.Cmd {
	return func() tea.Msg {
		models, err := hfSearchModels(query)
		return tuiHFSearchMsg{Models: models, Err: err}
	}
}

func (m tuiModel) hfFilesCmd(repo string) tea.Cmd {
	return func() tea.Msg {
		files, err := hfModelFiles(repo)
		return tuiHFFilesMsg{Repo: repo, Files: files, Err: err}
	}
}

func (m tuiModel) chatCmd(messages []map[string]string) tea.Cmd {
	messages = append([]map[string]string(nil), messages...)
	return func() tea.Msg {
		start := time.Now()
		reply, tokens, err := m.state.chatOnce(messages)
		return tuiChatMsg{Reply: reply, Tokens: tokens, Ms: time.Since(start).Milliseconds(), Err: err}
	}
}

func (m tuiModel) chatStreamCmd(messages []map[string]string, ch chan<- tuiChatStreamMsg) tea.Cmd {
	messages = append([]map[string]string(nil), messages...)
	return func() tea.Msg {
		m.state.chatStream(messages, ch)
		return nil
	}
}

func listenChatStreamCmd(ch <-chan tuiChatStreamMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return tuiChatStreamMsg{Done: true}
		}
		return msg
	}
}

func (m tuiModel) busyCmd(label string, fn func()) tea.Cmd {
	return func() tea.Msg {
		fn()
		return tuiBusyDone{Label: label, Snap: m.state.snapshot()}
	}
}

func (m tuiModel) consoleInstallCmd(force bool) tea.Cmd {
	return func() tea.Msg {
		logPath := processLogPath("install")
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			m.state.addLog("install log file failed: %v", err)
			m.state.installDeps(false)
			return tuiBusyDone{Label: "install", Snap: m.state.snapshot(), Err: err}
		}
		defer f.Close()
		_, _ = fmt.Fprintf(f, "ClusterKit install / repair started %s\n", time.Now().Format(time.RFC3339))
		openLogTerminal("ClusterKit install logs", logPath)
		m.state.installDepsWithOutput(force, f)
		_, _ = fmt.Fprintf(f, "\nDone. You can close this log window.\n")
		return tuiBusyDone{Label: "install", Snap: m.state.snapshot()}
	}
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tuiTick:
		if m.busy {
			return m, tuiTickCmd()
		}
		return m, tea.Batch(tuiTickCmd(), m.snapshotCmd())
	case tuiSnapshotMsg:
		m.snap = map[string]any(msg)
		return m, nil
	case tuiBusyDone:
		m.busy = false
		m.busyText = ""
		if msg.Snap != nil {
			m.snap = msg.Snap
		}
		return m, nil
	case tuiHFSearchMsg:
		m.busy = false
		m.busyText = ""
		if msg.Err != nil {
			m.state.addLog("model search failed: %v", msg.Err)
		} else {
			m.hfModels = msg.Models
			m.hfSel = 0
			m.hfOffset = 0
			m.browser = "repos"
		}
		return m, nil
	case tuiHFFilesMsg:
		m.busy = false
		m.busyText = ""
		if msg.Err != nil {
			m.state.addLog("model files failed: %v", msg.Err)
		} else {
			m.hfRepo = msg.Repo
			m.hfFiles = msg.Files
			m.fileSel = 0
			m.fileOffset = 0
			m.browser = "files"
		}
		return m, nil
	case tuiChatMsg:
		m.busy = false
		m.busyText = ""
		if msg.Err != nil {
			m.chat = append(m.chat, chatLine{Role: "error", Text: msg.Err.Error(), Ms: msg.Ms, At: time.Now()})
		} else {
			if msg.Tokens > 0 {
				m.chatUsed = msg.Tokens
			}
			m.chat = append(m.chat, chatLine{Role: "assistant", Text: msg.Reply, Tokens: msg.Tokens, Ms: msg.Ms, At: time.Now()})
		}
		return m, nil
	case tuiChatStreamMsg:
		if msg.Err != nil {
			m.busy = false
			m.busyText = ""
			m.chatStream = nil
			m.chat = append(m.chat, chatLine{Role: "error", Text: msg.Err.Error(), Ms: msg.Ms, At: time.Now()})
			return m, nil
		}
		if msg.Thought != "" && m.showThinking() {
			m.appendThoughtDelta(msg.Thought, msg.Tokens, msg.Ms)
		}
		if msg.Content != "" {
			m.appendAssistantDelta(msg.Content, msg.Tokens, msg.Ms)
			if msg.Tokens > 0 {
				m.chatUsed = msg.Tokens
			}
		}
		if msg.Done {
			m.busy = false
			m.busyText = ""
			m.chatStream = nil
			m.finishAssistantStream(msg.Tokens, msg.Ms)
			if msg.Tokens > 0 {
				m.chatUsed = msg.Tokens
			}
			return m, nil
		}
		if m.chatStream != nil {
			return m, listenChatStreamCmd(m.chatStream)
		}
		return m, nil
	case tea.KeyMsg:
		if m.busy {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			}
			return m, nil
		}
		if m.rolePick {
			switch msg.String() {
			case "ctrl+c", "esc", "q":
				return m, tea.Quit
			case "up", "left", "shift+tab", "k":
				m.roleSel = (m.roleSel + 1) % 2
			case "down", "right", "tab", "j":
				m.roleSel = (m.roleSel + 1) % 2
			case "1", "w":
				m.roleSel = 0
				m.applyRole("worker")
				m.rolePick = false
				if runtime.GOOS == "windows" {
					m.workerDevicePick = true
				}
				return m, m.snapshotCmd()
			case "2", "c":
				m.roleSel = 1
				m.applyRole("coordinator")
				m.rolePick = false
				return m, m.snapshotCmd()
			case "enter", " ":
				if m.roleSel == 0 {
					m.applyRole("worker")
					if runtime.GOOS == "windows" {
						m.workerDevicePick = true
					}
				} else {
					m.applyRole("coordinator")
				}
				m.rolePick = false
				return m, m.snapshotCmd()
			}
			return m, nil
		}
		if m.workerDevicePick {
			return m.updateWorkerDevicePick(msg)
		}
		if m.screen == "models" {
			return m.updateModelScreen(msg)
		}
		if m.screen == "settings" {
			return m.updateSettingsScreen(msg)
		}
		if m.screen == "api" {
			return m.updateAPIScreen(msg)
		}
		if m.screen == "workerLayers" {
			return m.updateWorkerLayersScreen(msg)
		}
		if m.screen == "chat" {
			return m.updateChatScreen(msg)
		}
		switch msg.String() {
		case "ctrl+c", "esc", "q":
			return m, tea.Quit
		case "up", "left", "shift+tab", "k":
			m.moveSelection(-1)
		case "down", "right", "tab", "j":
			m.moveSelection(1)
		case "home":
			m.selected = m.firstAllowedAction()
		case "end":
			m.selected = m.lastAllowedAction()
		case "enter", " ":
			if m.actions[m.selected].Key == "q" {
				return m, tea.Quit
			}
			return m.runSelectedAction()
		default:
			key := strings.ToLower(msg.String())
			for i, a := range m.actions {
				if key == a.Key && m.actionAllowed(a.Key) {
					m.selected = i
					if a.Key == "q" {
						return m, tea.Quit
					}
					return m.runSelectedAction()
				}
			}
		}
	}
	return m, nil
}

func (m *tuiModel) applyRole(role string) {
	m.state.mu.Lock()
	cfg := m.state.config
	cfg.Role = role
	cfg.RoleExplicit = true
	m.state.config = cfg
	m.state.mu.Unlock()
	_ = saveConfig(cfg)
	m.snap = m.state.fastSnapshot()
	m.state.addLog("role selected: %s", role)
	m.selected = m.firstAllowedAction()
	if role == "worker" && runtime.GOOS != "windows" {
		go m.state.autostartWorker()
	}
}

func (m tuiModel) updateWorkerDevicePick(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc", "q":
		return m, tea.Quit
	case "up", "left", "shift+tab", "k":
		m.workerDeviceSel = (m.workerDeviceSel + 1) % 2
	case "down", "right", "tab", "j":
		m.workerDeviceSel = (m.workerDeviceSel + 1) % 2
	case "1", "g":
		m.workerDeviceSel = 0
		m.applyWorkerDevice("gpu")
		return m, m.snapshotCmd()
	case "2", "c":
		m.workerDeviceSel = 1
		m.applyWorkerDevice("cpu")
		return m, m.snapshotCmd()
	case "enter", " ":
		if m.workerDeviceSel == 0 {
			m.applyWorkerDevice("gpu")
		} else {
			m.applyWorkerDevice("cpu")
		}
		return m, m.snapshotCmd()
	}
	return m, nil
}

func (m *tuiModel) applyWorkerDevice(mode string) {
	m.state.mu.Lock()
	cfg := m.state.config
	cfg.ComputeMode = mode
	m.state.config = cfg
	m.state.mu.Unlock()
	_ = saveConfig(cfg)
	m.workerDevicePick = false
	m.snap = m.state.fastSnapshot()
	m.state.addLog("worker compute device selected: %s", mode)
	go m.state.autostartWorker()
}

func (m tuiModel) currentRole() string {
	snap := m.snap
	if snap == nil {
		snap = m.state.fastSnapshot()
	}
	if cfg, ok := snap["config"].(Config); ok {
		if cfg.RoleExplicit {
			return cfg.Role
		}
	}
	return ""
}

func (m tuiModel) actionAllowed(key string) bool {
	role := strings.ToLower(m.currentRole())
	if role == "worker" {
		return key == "i" || key == "w" || key == "a" || key == "q"
	}
	if m.coordinatorRunning() {
		return key == "c" || key == "a" || key == "t" || key == "q"
	}
	return key != "w"
}

func (m tuiModel) coordinatorRunning() bool {
	snap := m.snap
	if snap == nil {
		snap = m.state.fastSnapshot()
	}
	running, _ := snap["coordinatorRunning"].(bool)
	return running
}

func (m *tuiModel) moveSelection(delta int) {
	if len(m.actions) == 0 {
		return
	}
	for i := 0; i < len(m.actions); i++ {
		m.selected = (m.selected + delta + len(m.actions)) % len(m.actions)
		if m.actionAllowed(m.actions[m.selected].Key) {
			return
		}
	}
}

func (m tuiModel) firstAllowedAction() int {
	for i, a := range m.actions {
		if m.actionAllowed(a.Key) {
			return i
		}
	}
	return 0
}

func (m tuiModel) lastAllowedAction() int {
	for i := len(m.actions) - 1; i >= 0; i-- {
		if m.actionAllowed(m.actions[i].Key) {
			return i
		}
	}
	return 0
}

func (m tuiModel) runSelectedAction() (tea.Model, tea.Cmd) {
	a := m.actions[m.selected]
	if !m.actionAllowed(a.Key) {
		return m, nil
	}
	switch a.Key {
	case "i":
		m.busy = true
		m.busyText = "Installing dependencies… console output is active"
		return m, m.consoleInstallCmd(false)
	case "d":
		m.busy = true
		m.busyText = "Discovering workers on LAN…"
		return m, m.busyCmd("discover", a.Run)
	case "m":
		m.screen = "models"
		m.browser = "local"
		return m, nil
	case "b":
		m.screen = "models"
		m.browser = "repos"
		m.hfQuery = "qwen gguf"
		m.busy = true
		m.busyText = "Searching Hugging Face models…"
		return m, m.hfSearchCmd(m.hfQuery)
	case "s":
		m.screen = "settings"
		return m, nil
	case "a":
		m.screen = "api"
		return m, nil
	case "l":
		m.screen = "workerLayers"
		return m, nil
	case "t":
		m.screen = "chat"
		m.chatIn.Focus()
		return m, nil
	default:
		go a.Run()
		return m, m.snapshotCmd()
	}
}

func (m tuiModel) updateChatScreen(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.busy {
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.screen = ""
		m.chatIn.Blur()
		return m, nil
	case "ctrl+k":
		m.chat = nil
		m.chatUsed = 0
		m.chatIn.SetValue("")
		return m, nil
	case "enter":
		prompt := strings.TrimSpace(m.chatIn.Value())
		if prompt == "" {
			return m, nil
		}
		m.chat = append(m.chat, chatLine{Role: "user", Text: prompt, At: time.Now()})
		messages := chatMessagesForModel(m.chat)
		m.chatIn.SetValue("")
		m.busy = true
		m.busyText = "Generating reply… live stream active"
		m.chat = append(m.chat, chatLine{Role: "assistant", Text: "", At: time.Now()})
		ch := make(chan tuiChatStreamMsg, 128)
		m.chatStream = ch
		return m, tea.Batch(m.chatStreamCmd(messages, ch), listenChatStreamCmd(ch))
	}
	var cmd tea.Cmd
	m.chatIn, cmd = m.chatIn.Update(msg)
	return m, cmd
}

func (m tuiModel) showThinking() bool {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	return !m.state.config.HideThinking
}

func (m *tuiModel) appendAssistantDelta(delta string, tokens int, ms int64) {
	for i := len(m.chat) - 1; i >= 0; i-- {
		if m.chat[i].Role == "assistant" {
			m.chat[i].Text += delta
			m.chat[i].Tokens = tokens
			m.chat[i].Ms = ms
			if m.chat[i].At.IsZero() {
				m.chat[i].At = time.Now()
			}
			return
		}
	}
	m.chat = append(m.chat, chatLine{Role: "assistant", Text: delta, Tokens: tokens, Ms: ms, At: time.Now()})
}

func (m *tuiModel) appendThoughtDelta(delta string, tokens int, ms int64) {
	if strings.TrimSpace(delta) == "" {
		return
	}
	for i := len(m.chat) - 1; i >= 0; i-- {
		if m.chat[i].Role == "assistant" {
			if !strings.Contains(m.chat[i].Text, thinkingHeader()) {
				m.chat[i].Text += thinkingHeader() + "\n"
			}
			m.chat[i].Text += delta
			m.chat[i].Tokens = tokens
			m.chat[i].Ms = ms
			if m.chat[i].At.IsZero() {
				m.chat[i].At = time.Now()
			}
			return
		}
	}
	m.chat = append(m.chat, chatLine{Role: "assistant", Text: thinkingHeader() + "\n" + delta, Tokens: tokens, Ms: ms, At: time.Now()})
}

func (m *tuiModel) finishAssistantStream(tokens int, ms int64) {
	for i := len(m.chat) - 1; i >= 0; i-- {
		if m.chat[i].Role == "assistant" {
			if strings.TrimSpace(m.chat[i].Text) == "" {
				m.chat[i].Text = "[empty response]"
			}
			if tokens > 0 {
				m.chat[i].Tokens = tokens
			}
			if ms > 0 {
				m.chat[i].Ms = ms
			}
			return
		}
	}
}

func (m tuiModel) updateSettingsScreen(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.coordinatorRunning() {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "q":
			m.screen = ""
			return m, nil
		}
		return m, nil
	}
	settings := launchSettingKeys()
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "q":
		m.screen = ""
		return m, nil
	case "up", "k", "shift+tab":
		m.settingSel = (m.settingSel - 1 + len(settings)) % len(settings)
	case "down", "j", "tab":
		m.settingSel = (m.settingSel + 1) % len(settings)
	case "shift+left", "H":
		m.adjustSettingFine(settings[m.settingSel], -1)
		m.snap = m.state.fastSnapshot()
	case "shift+right", "L":
		m.adjustSettingFine(settings[m.settingSel], 1)
		m.snap = m.state.fastSnapshot()
	case "left", "h":
		m.adjustSetting(settings[m.settingSel], -1)
		m.snap = m.state.fastSnapshot()
	case "right", "l", "enter", " ":
		if settings[m.settingSel] == "model" {
			m.screen = "models"
			m.browser = "local"
			return m, nil
		}
		m.adjustSetting(settings[m.settingSel], 1)
		m.snap = m.state.fastSnapshot()
	}
	return m, nil
}

func (m tuiModel) updateAPIScreen(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "q":
		m.screen = ""
		return m, nil
	case "r", "R":
		m.snap = m.state.fastSnapshot()
		return m, nil
	}
	return m, nil
}

func (m tuiModel) updateWorkerLayersScreen(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.coordinatorRunning() {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "q":
			m.screen = ""
			return m, nil
		}
		return m, nil
	}
	m.state.mu.Lock()
	n := len(m.state.config.Workers) + 1 // row 0 is LOCAL/coordinator
	m.state.mu.Unlock()
	if m.workerLayerSel >= n {
		m.workerLayerSel = n - 1
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "q":
		m.screen = ""
		return m, nil
	case "up", "k", "shift+tab":
		m.workerLayerSel = (m.workerLayerSel - 1 + n) % n
	case "down", "j", "tab":
		m.workerLayerSel = (m.workerLayerSel + 1) % n
	case "ctrl+up", "K":
		if m.workerLayerSel > 0 {
			m.moveWorkerOrder(m.workerLayerSel-1, -1)
			if m.workerLayerSel > 1 {
				m.workerLayerSel--
			}
			m.snap = m.state.fastSnapshot()
		}
	case "ctrl+down", "J":
		if m.workerLayerSel > 0 {
			m.moveWorkerOrder(m.workerLayerSel-1, 1)
			if m.workerLayerSel < n-1 {
				m.workerLayerSel++
			}
			m.snap = m.state.fastSnapshot()
		}
	case "left", "h":
		m.adjustLayerRow(m.workerLayerSel, -1)
		m.snap = m.state.fastSnapshot()
	case "right", "l", "enter", " ":
		m.adjustLayerRow(m.workerLayerSel, 1)
		m.snap = m.state.fastSnapshot()
	case "shift+left", "H":
		m.adjustLayerRow(m.workerLayerSel, -4)
		m.snap = m.state.fastSnapshot()
	case "shift+right", "L":
		m.adjustLayerRow(m.workerLayerSel, 4)
		m.snap = m.state.fastSnapshot()
	case "0":
		m.setLayerRow(m.workerLayerSel, 0)
		m.snap = m.state.fastSnapshot()
	case "e":
		if m.workerLayerSel > 0 {
			m.toggleWorkerEnabled(m.workerLayerSel - 1)
		}
		m.snap = m.state.fastSnapshot()
	case "a":
		m.autoSeedWorkerLayers()
		m.snap = m.state.fastSnapshot()
	case "r":
		m.resetWorkerLayers()
		m.snap = m.state.fastSnapshot()
	case "d", "D":
		m.busy = true
		m.busyText = "Discovering workers on LAN…"
		return m, m.busyCmd("discover", func() { tuiDiscover(m.state) })
	}
	return m, nil
}

func (m tuiModel) adjustWorkerLayers(idx, delta int) {
	m.state.mu.Lock()
	cfg := m.state.config
	if idx >= 0 && idx < len(cfg.Workers) {
		cfg.Workers[idx].Layers = clampInt(cfg.Workers[idx].Layers+delta, 0, 999)
		cfg.Workers[idx].SplitWeight = float64(cfg.Workers[idx].Layers)
		cfg = applyManualWorkerLayersToConfig(cfg)
		m.state.config = cfg
	}
	m.state.mu.Unlock()
	_ = saveConfig(cfg)
	m.state.addLog("worker layers updated: %s", layerPlanSummary(cfg, cfg.Workers))
}

func (m tuiModel) adjustLayerRow(row, delta int) {
	if row == 0 {
		m.state.mu.Lock()
		cfg := m.state.config
		cfg.CoordinatorLayers = clampInt(cfg.CoordinatorLayers+delta, 0, 999)
		cfg = applyManualWorkerLayersToConfig(cfg)
		m.state.config = cfg
		m.state.mu.Unlock()
		_ = saveConfig(cfg)
		m.state.addLog("coordinator layers updated: %s", layerPlanSummary(cfg, cfg.Workers))
		return
	}
	m.adjustWorkerLayers(row-1, delta)
}

func (m tuiModel) setLayerRow(row, layers int) {
	if row == 0 {
		m.state.mu.Lock()
		cfg := m.state.config
		cfg.CoordinatorLayers = clampInt(layers, 0, 999)
		cfg = applyManualWorkerLayersToConfig(cfg)
		m.state.config = cfg
		m.state.mu.Unlock()
		_ = saveConfig(cfg)
		m.state.addLog("coordinator layers updated: %s", layerPlanSummary(cfg, cfg.Workers))
		return
	}
	m.setWorkerLayers(row-1, layers)
}

func (m tuiModel) moveWorkerOrder(idx, delta int) {
	m.state.mu.Lock()
	cfg := m.state.config
	to := idx + delta
	if idx >= 0 && idx < len(cfg.Workers) && to >= 0 && to < len(cfg.Workers) {
		cfg.Workers[idx], cfg.Workers[to] = cfg.Workers[to], cfg.Workers[idx]
		cfg = applyManualWorkerLayersToConfig(cfg)
		m.state.config = cfg
	}
	m.state.mu.Unlock()
	_ = saveConfig(cfg)
	m.state.addLog("worker order updated: %s", workerOrderSummary(cfg.Workers))
}

func workerOrderSummary(workers []Worker) string {
	parts := make([]string, 0, len(workers))
	for i, wk := range workers {
		parts = append(parts, fmt.Sprintf("RPC%d=%s", i, short(firstNonEmpty(wk.Name, wk.Host), 12)))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

func (m tuiModel) setWorkerLayers(idx, layers int) {
	m.state.mu.Lock()
	cfg := m.state.config
	if idx >= 0 && idx < len(cfg.Workers) {
		cfg.Workers[idx].Layers = clampInt(layers, 0, 999)
		cfg.Workers[idx].SplitWeight = float64(cfg.Workers[idx].Layers)
		cfg = applyManualWorkerLayersToConfig(cfg)
		m.state.config = cfg
	}
	m.state.mu.Unlock()
	_ = saveConfig(cfg)
	m.state.addLog("worker layers updated: %s", layerPlanSummary(cfg, cfg.Workers))
}

func (m tuiModel) toggleWorkerEnabled(idx int) {
	m.state.mu.Lock()
	cfg := m.state.config
	if idx >= 0 && idx < len(cfg.Workers) {
		cfg.Workers[idx].Disabled = !cfg.Workers[idx].Disabled
		cfg = applyManualWorkerLayersToConfig(cfg)
		m.state.config = cfg
	}
	m.state.mu.Unlock()
	_ = saveConfig(cfg)
	m.state.addLog("worker enabled toggled: %s", layerPlanSummary(cfg, cfg.Workers))
}

func (m tuiModel) resetWorkerLayers() {
	m.state.mu.Lock()
	cfg := m.state.config
	cfg.CoordinatorLayers = 0
	for i := range cfg.Workers {
		cfg.Workers[i].Layers = 0
		cfg.Workers[i].SplitWeight = 0
	}
	m.state.config = cfg
	m.state.mu.Unlock()
	_ = saveConfig(cfg)
	m.state.addLog("manual layer plan cleared")
}

func (m tuiModel) autoSeedWorkerLayers() {
	m.state.mu.Lock()
	cfg := m.state.config
	if len(cfg.Workers) == 0 {
		m.state.mu.Unlock()
		return
	}
	total := cfg.GPULayers
	if total <= 0 {
		total = 4
	}
	weights := make([]float64, len(cfg.Workers))
	sum := 0.0
	for i, w := range cfg.Workers {
		if w.Disabled {
			continue
		}
		weights[i] = workerUsableGB(w)
		if weights[i] <= 0 {
			weights[i] = 1
		}
		sum += weights[i]
	}
	allocated := 0
	last := -1
	for i := range cfg.Workers {
		if cfg.Workers[i].Disabled || sum <= 0 {
			cfg.Workers[i].Layers = 0
			cfg.Workers[i].SplitWeight = 0
			continue
		}
		last = i
		layers := int(float64(total) * weights[i] / sum)
		cfg.Workers[i].Layers = layers
		allocated += layers
	}
	if last >= 0 {
		cfg.Workers[last].Layers += total - allocated
	}
	for i := range cfg.Workers {
		cfg.Workers[i].SplitWeight = float64(cfg.Workers[i].Layers)
	}
	cfg = applyManualWorkerLayersToConfig(cfg)
	m.state.config = cfg
	m.state.mu.Unlock()
	_ = saveConfig(cfg)
	m.state.addLog("auto worker layer seed: %s", layerPlanSummary(cfg, cfg.Workers))
}

func launchSettingKeys() []string {
	return []string{"model", "context", "batch", "ubatch", "parallel", "threads", "gpuLayers", "cacheRam", "chatTimeout", "chatMaxTokens", "chatNoTokenLimit", "showThinking", "splitMode", "tensorSplit", "computeMode", "memoryMode", "coordinatorLocal"}
}

func (m tuiModel) adjustSetting(key string, dir int) {
	m.state.mu.Lock()
	cfg := m.state.config
	m.state.mu.Unlock()
	maxCtx := 32768
	if strings.TrimSpace(cfg.ModelPath) != "" {
		if v, err := ggufMaxContext(cfg.ModelPath); err == nil && v > 0 {
			maxCtx = v
		}
	}
	switch key {
	case "model":
		return
	case "context":
		cfg.Context = stepPow2(cfg.Context, dir, 512, maxCtx)
	case "batch":
		cfg.Batch = stepPow2(cfg.Batch, dir, 32, 4096)
	case "ubatch":
		cfg.UBatch = stepPow2(cfg.UBatch, dir, 16, 2048)
	case "parallel":
		cfg.Parallel = clampInt(cfg.Parallel+dir, 1, 8)
	case "threads":
		cfg.Threads = clampInt(cfg.Threads+dir, 1, runtime.NumCPU())
	case "gpuLayers":
		cfg.GPULayers = clampInt(cfg.GPULayers+dir, 0, 999)
	case "cacheRam":
		cfg.CacheRAM = clampInt(cfg.CacheRAM+dir*512, 0, 32768)
	case "chatTimeout":
		cfg.ChatTimeout = clampInt(cfg.ChatTimeout+dir*60, 60, 86400)
	case "chatMaxTokens":
		cfg.ChatMaxTokens = stepPow2(cfg.ChatMaxTokens, dir, 64, 32768)
	case "chatNoTokenLimit":
		cfg.ChatNoTokenLimit = !cfg.ChatNoTokenLimit
	case "showThinking":
		cfg.HideThinking = !cfg.HideThinking
	case "splitMode":
		modes := []string{"layer", "row", "tensor", "none"}
		idx := 0
		for i, v := range modes {
			if strings.EqualFold(splitMode(cfg), v) {
				idx = i
			}
		}
		cfg.SplitMode = modes[(idx+dir+len(modes))%len(modes)]
	case "tensorSplit":
		cfg = clearManualWorkerLayers(cfg)
		cfg.TensorSplit = nextTensorSplitPreset(cfg, dir)
	case "computeMode":
		modes := []string{"auto", "gpu", "cpu"}
		idx := 0
		for i, v := range modes {
			if strings.EqualFold(cfg.ComputeMode, v) {
				idx = i
			}
		}
		cfg.ComputeMode = modes[(idx+dir+len(modes))%len(modes)]
	case "coordinatorLocal":
		cfg.CoordinatorLocal = !cfg.CoordinatorLocal
	case "memoryMode":
		modes := []string{"normal", "mmap", "safest"}
		idx := 0
		for i, v := range modes {
			if strings.EqualFold(cfg.MemoryMode, v) {
				idx = i
			}
		}
		cfg.MemoryMode = modes[(idx+dir+len(modes))%len(modes)]
		if cfg.MemoryMode == "safest" {
			if cfg.Batch > 128 {
				cfg.Batch = 128
			}
			if cfg.UBatch > 64 {
				cfg.UBatch = 64
			}
			if cfg.Parallel > 1 {
				cfg.Parallel = 1
			}
		}
	}
	m.state.mu.Lock()
	m.state.config = cfg
	m.state.mu.Unlock()
	_ = saveConfig(cfg)
	m.state.addLog("setting %s updated", key)
}

func (m tuiModel) adjustSettingFine(key string, dir int) {
	if key == "chatMaxTokens" {
		m.state.mu.Lock()
		cfg := m.state.config
		m.state.mu.Unlock()
		cfg.ChatMaxTokens = stepLinear(cfg.ChatMaxTokens, dir, 100, 1, 32768)
		m.state.mu.Lock()
		m.state.config = cfg
		m.state.mu.Unlock()
		_ = saveConfig(cfg)
		m.state.addLog("setting %s fine-adjusted", key)
		return
	}
	if key != "context" {
		m.adjustSetting(key, dir)
		return
	}
	m.state.mu.Lock()
	cfg := m.state.config
	m.state.mu.Unlock()
	maxCtx := 32768
	if strings.TrimSpace(cfg.ModelPath) != "" {
		if v, err := ggufMaxContext(cfg.ModelPath); err == nil && v > 0 {
			maxCtx = v
		}
	}
	cfg.Context = stepLinear(cfg.Context, dir, 500, 500, maxCtx)
	m.state.mu.Lock()
	m.state.config = cfg
	m.state.mu.Unlock()
	_ = saveConfig(cfg)
	m.state.addLog("setting %s fine-adjusted", key)
}

func stepPow2(v, dir, minV, maxV int) int {
	if v <= 0 {
		v = minV
	}
	if dir > 0 {
		v *= 2
	} else {
		v /= 2
	}
	return clampInt(v, minV, maxV)
}

func stepLinear(v, dir, step, minV, maxV int) int {
	if v <= 0 {
		v = minV
	}
	return clampInt(v+dir*step, minV, maxV)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (m tuiModel) updateModelScreen(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.coordinatorRunning() {
		m.screen = ""
		m.browser = ""
		m.state.addLog("model changes are locked while coordinator is running")
		return m, nil
	}
	if m.hfSearchFocus {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.hfSearchFocus = false
			m.hfSearchIn.Blur()
			return m, nil
		case "enter":
			m.hfSearchFocus = false
			m.hfSearchIn.Blur()
			m.hfQuery = strings.TrimSpace(m.hfSearchIn.Value())
			m.browser = "repos"
			m.busy = true
			m.busyText = "Searching Hugging Face models…"
			return m, m.hfSearchCmd(defaultHFQuery(m.hfQuery))
		}
		var cmd tea.Cmd
		m.hfSearchIn, cmd = m.hfSearchIn.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "ctrl+c", "q", "esc":
		m.screen = ""
		m.browser = ""
		return m, nil
	case "tab":
		if m.browser == "local" {
			m.browser = "repos"
			if len(m.hfModels) == 0 {
				m.busy = true
				m.busyText = "Searching Hugging Face models…"
				return m, m.hfSearchCmd(defaultHFQuery(m.hfQuery))
			}
		} else {
			m.browser = "local"
		}
	case "up", "k":
		m.moveModelSelection(-1)
	case "down", "j":
		m.moveModelSelection(1)
	case "left", "h":
		if m.browser == "files" {
			m.browser = "repos"
		} else {
			m.browser = "local"
		}
	case "right", "l":
		if m.browser == "repos" && len(m.hfModels) > 0 {
			repo := m.hfModels[m.hfSel].ID
			m.busy = true
			m.busyText = "Loading model files…"
			return m, m.hfFilesCmd(repo)
		}
	case "enter", " ":
		switch m.browser {
		case "local":
			models := m.state.localModels()
			if len(models) > 0 {
				if m.localSel >= len(models) {
					m.localSel = len(models) - 1
				}
				m.state.selectModelPath(models[m.localSel].Path)
				m.snap = m.state.fastSnapshot()
			}
		case "repos":
			if len(m.hfModels) > 0 {
				repo := m.hfModels[m.hfSel].ID
				m.busy = true
				m.busyText = "Loading model files…"
				return m, m.hfFilesCmd(repo)
			}
		case "files":
			if len(m.hfFiles) > 0 {
				file := m.hfFiles[m.fileSel].RFilename
				m.busy = true
				m.busyText = "Downloading model… this can take a while"
				return m, m.busyCmd("download", func() { m.state.downloadModel(m.hfRepo, file) })
			}
		}
	case "r":
		m.busy = true
		m.busyText = "Searching Hugging Face models…"
		return m, m.hfSearchCmd(defaultHFQuery(m.hfQuery))
	case "/":
		m.browser = "repos"
		m.hfSearchFocus = true
		m.hfSearchIn.Focus()
		return m, textinput.Blink
	}
	return m, nil
}

func (m *tuiModel) moveModelSelection(delta int) {
	visible := max(4, m.height-12)
	switch m.browser {
	case "local":
		n := len(m.state.localModels())
		if n > 0 {
			m.localSel = (m.localSel + delta + n) % n
			m.localOffset = adjustScroll(m.localSel, m.localOffset, visible, n)
		}
	case "repos":
		if len(m.hfModels) > 0 {
			m.hfSel = (m.hfSel + delta + len(m.hfModels)) % len(m.hfModels)
			m.hfOffset = adjustScroll(m.hfSel, m.hfOffset, visible, len(m.hfModels))
		}
	case "files":
		if len(m.hfFiles) > 0 {
			m.fileSel = (m.fileSel + delta + len(m.hfFiles)) % len(m.hfFiles)
			m.fileOffset = adjustScroll(m.fileSel, m.fileOffset, visible, len(m.hfFiles))
		}
	}
}

func adjustScroll(sel, offset, visible, total int) int {
	if visible < 1 {
		visible = 1
	}
	if sel < offset {
		offset = sel
	}
	if sel >= offset+visible {
		offset = sel - visible + 1
	}
	maxOffset := max(0, total-visible)
	return clampInt(offset, 0, maxOffset)
}

func defaultHFQuery(q string) string {
	if strings.TrimSpace(q) == "" {
		return "qwen gguf"
	}
	return q
}

func memoryMode(cfg Config) string {
	mode := strings.ToLower(strings.TrimSpace(cfg.MemoryMode))
	switch mode {
	case "mmap", "safest":
		return mode
	default:
		return "normal"
	}
}

func splitMode(cfg Config) string {
	mode := strings.ToLower(strings.TrimSpace(cfg.SplitMode))
	switch mode {
	case "none", "layer", "row", "tensor":
		return mode
	default:
		return "layer"
	}
}

func isCoordinatorRole(cfg Config) bool {
	return cfg.RoleExplicit && strings.EqualFold(cfg.Role, "coordinator")
}

func isWorkerRole(cfg Config) bool {
	return cfg.RoleExplicit && strings.EqualFold(cfg.Role, "worker")
}

func tuiDiscover(s *AppState) {
	peers := discoverPeers(1400 * time.Millisecond)
	workers := make([]Worker, 0, len(peers))
	s.mu.Lock()
	previous := append([]Worker(nil), s.config.Workers...)
	s.mu.Unlock()
	for _, p := range peers {
		w := Worker{Name: p.Name, Host: p.Host, Port: p.Port, OS: p.OS, Arch: p.Arch, RAMBytes: p.RAM, VRAMBytes: p.VRAMBytes, Backend: p.Backend, Threads: p.Threads, CrashCount: p.CrashCount, Stability: p.Stability, RSSBytes: p.RSSBytes, LoadPct: p.LoadPct, Status: "discovered"}
		if w.Port == 0 {
			w.Port = 50052
		}
		workers = append(workers, w)
	}
	workers = mergeManualWorkerSettings(workers, previous)
	workers = s.classifyWorkers(workers, 650*time.Millisecond)
	s.mu.Lock()
	cfg := s.config
	cfg.Workers = workers
	s.config = cfg
	s.mu.Unlock()
	_ = saveConfig(cfg)
	s.addLog("discovered %d worker(s)", len(workers))
}

func mergeManualWorkerSettings(workers, previous []Worker) []Worker {
	byKey := map[string]Worker{}
	byHost := map[string]Worker{}
	for _, old := range previous {
		if old.Host == "" {
			continue
		}
		byHost[old.Host] = old
		p := old.Port
		if p == 0 {
			p = 50052
		}
		byKey[fmt.Sprintf("%s:%d", old.Host, p)] = old
	}
	for i := range workers {
		p := workers[i].Port
		if p == 0 {
			p = 50052
		}
		old, ok := byKey[fmt.Sprintf("%s:%d", workers[i].Host, p)]
		if !ok {
			old, ok = byHost[workers[i].Host]
		}
		if !ok {
			continue
		}
		if strings.TrimSpace(old.Name) != "" {
			workers[i].Name = old.Name
		}
		workers[i].Disabled = old.Disabled
		workers[i].Layers = old.Layers
		workers[i].SplitWeight = old.SplitWeight
	}
	return workers
}

func (m tuiModel) View() string {
	s := m.styles
	width := m.width
	if width < 76 {
		width = 76
	}
	if width > 118 {
		width = 118
	}
	boxW := width - 2
	if m.rolePick {
		return s.base.Render(m.rolePickerView(boxW))
	}
	if m.workerDevicePick {
		return s.base.Render(m.workerDevicePickerView(boxW))
	}
	if m.screen == "models" {
		return s.base.Render(m.modelBrowserView(boxW))
	}
	if m.screen == "settings" {
		return s.base.Render(m.settingsView(boxW))
	}
	if m.screen == "api" {
		return s.base.Render(m.apiView(boxW))
	}
	if m.screen == "workerLayers" {
		return s.base.Render(m.workerLayersView(boxW))
	}
	if m.screen == "chat" {
		return s.base.Render(m.chatView(boxW))
	}
	snap := m.snap
	if snap == nil {
		snap = m.state.fastSnapshot()
	}
	cfg := snap["config"].(Config)
	if cfg.Role == "worker" {
		return s.base.Render(m.workerView(boxW, snap, cfg))
	}

	cluster := m.box("ClusterKit", boxW, []string{
		"local llama.cpp cluster",
		fmt.Sprintf("role %-11s node %-15s system %s/%s", cfg.Role, snap["localIP"], snap["os"], snap["arch"]),
		"steps: install → discover → tune manually → start",
	})
	status := m.box("Status", boxW, []string{
		fmt.Sprintf("llama.cpp %-9s compute %-7s api 127.0.0.1:%s", yesNo(snap["llamaReady"].(bool)), cfg.ComputeMode, portOnly(m.addr)),
		fmt.Sprintf("worker %-9s coordinator %s model %s", yesNo(snap["workerRunning"].(bool)), m.coordinatorStatusBadge(snap), m.modelStatusBadge(safeString(snap["modelStatus"]))),
		fmt.Sprintf("model detail: %s", safeString(snap["serverLoad"])),
		fmt.Sprintf("ctx %-6d batch %-5d ubatch %-5d parallel %-2d", cfg.Context, cfg.Batch, cfg.UBatch, cfg.Parallel),
	})
	clusterPanel := m.clusterCapacityPanel(boxW, snap, cfg)
	launchPanel := m.launchSettingsPanel(boxW, cfg)

	menuRows := []string{}
	for i, a := range m.actions {
		if !m.actionAllowed(a.Key) {
			continue
		}
		label := m.actionLabel(a, snap)
		row := fmt.Sprintf("❯ [%s] %s", strings.ToUpper(a.Key), label)
		if i == m.selected {
			menuRows = append(menuRows, s.selected.Render(padRight(row, boxW-6)))
		} else {
			menuRows = append(menuRows, "  "+s.muted.Render(fmt.Sprintf("[%s]", strings.ToUpper(a.Key)))+" "+label)
		}
	}
	menu := m.box("Menu", boxW, menuRows)

	workers, _ := snap["workerStatuses"].([]Worker)
	workerRows := []string{}
	if len(workers) == 0 {
		workerRows = append(workerRows, s.muted.Render("no workers discovered yet — press D to scan"))
	}
	workerBudget := max(3, m.height-25)
	for i, w := range workers {
		if i >= workerBudget {
			workerRows = append(workerRows, s.muted.Render(fmt.Sprintf("… %d more", len(workers)-i)))
			break
		}
		status := m.statusStyle(w.Status).Render(padRight(short(w.Status, 12), 12))
		rpc := s.bad.Render("rpc no")
		if w.OK && w.Status == "connected" {
			rpc = s.ok.Render("rpc yes")
		} else if w.Status == "busy/agent" {
			rpc = s.warn.Render("agent only")
		}
		layerLabel := fmt.Sprintf("L%-3d", w.Layers)
		if w.Disabled {
			layerLabel = "off"
		}
		workerRows = append(workerRows, fmt.Sprintf("%-18s %-15s %-12s %s %-10s %-4s RAM %7s VRAM %7s load %5.0f%% crash %d", short(w.Name, 18), short(w.Host, 15), short(w.Backend, 12), status, rpc, layerLabel, humanBytes(w.RAMBytes), humanBytes(w.VRAMBytes), w.LoadPct, w.CrashCount))
	}
	workersBox := m.box("Workers", boxW, workerRows)

	help := s.muted.Render(terminalText("↑/↓/←/→ or Tab: navigate   Enter: run   I/W/C/D/A/L: hotkeys   Esc/Q: quit"))
	parts := []string{cluster, status, clusterPanel, launchPanel, menu}
	if m.busy {
		parts = append(parts, m.loadingBox(boxW))
	}
	parts = append(parts, workersBox, help)
	return s.base.Render(lipgloss.JoinVertical(lipgloss.Left, parts...))
}

func (m tuiModel) rolePickerView(width int) string {
	s := m.styles
	roles := []string{"Worker — this machine shares compute", "Coordinator — this machine starts the model/chat"}
	rows := []string{
		"What should this machine do?",
		"",
	}
	for i, role := range roles {
		prefix := fmt.Sprintf("[%d] ", i+1)
		row := prefix + role
		if i == m.roleSel {
			row = s.selected.Render(padRight("❯ "+row, width-8))
		} else {
			row = "  " + row
		}
		rows = append(rows, row)
	}
	rows = append(rows, "", s.muted.Render("Use ↑/↓ then Enter, or press 1/2. Q exits."))
	return lipgloss.JoinVertical(lipgloss.Left,
		m.box("ClusterKit startup", width, []string{"local llama.cpp cluster", "role is asked on every launch so you don't start wrong mode"}),
		m.box("Choose role", width, rows),
	)
}

func (m tuiModel) workerDevicePickerView(width int) string {
	s := m.styles
	options := []string{"GPU — CUDA worker", "CPU — RAM/CPU worker"}
	rows := []string{"Windows worker: which device should this machine use?", ""}
	for i, opt := range options {
		row := fmt.Sprintf("[%d] %s", i+1, opt)
		if i == m.workerDeviceSel {
			row = s.selected.Render(padRight("❯ "+row, width-8))
		} else {
			row = "  " + row
		}
		rows = append(rows, row)
	}
	rows = append(rows, "", s.muted.Render("Use ↑/↓ then Enter, or press 1/2. Q exits."))
	return lipgloss.JoinVertical(lipgloss.Left,
		m.box("ClusterKit worker device", width, []string{"this choice is asked only on Windows workers"}),
		m.box("Choose compute device", width, rows),
	)
}

func (m tuiModel) workerView(width int, snap map[string]any, cfg Config) string {
	s := m.styles
	running, _ := snap["workerRunning"].(bool)
	status := s.bad.Render("● STOPPED")
	if running {
		status = s.ok.Render("● RUNNING")
	}
	hw := hardwareInfo()
	_, loadPct := m.state.workerLoadStats()
	install := installStatus(cfg)
	toolStatus := m.styles.bad.Render("missing")
	if install.Ready {
		toolStatus = m.styles.ok.Render("ready")
	}
	rows := []string{
		fmt.Sprintf("status      %s", status),
		fmt.Sprintf("tools       %s (%s) — %s", toolStatus, install.Mode, install.Reason),
		fmt.Sprintf("compute     %s", cfg.ComputeMode),
		fmt.Sprintf("endpoint    %s:%d", snap["localIP"], cfg.RPCPort),
		fmt.Sprintf("backend     %s", hw.Backend),
		fmt.Sprintf("system      %s/%s   threads %d", hw.OS, hw.Arch, hw.CPUCount),
		fmt.Sprintf("RAM         %s", humanBytes(hw.RAMBytes)),
		fmt.Sprintf("VRAM        %s", humanBytes(hw.VRAMBytes)),
		fmt.Sprintf("model load  %.0f%%", loadPct),
	}
	menu := m.workerMenuRows(running)
	return lipgloss.JoinVertical(lipgloss.Left,
		m.box("ClusterKit Worker", width, []string{"this machine only shares compute; coordinator controls model/chat"}),
		m.box("Worker status", width, rows),
		m.box("Controls", width, menu),
		s.muted.Render("↑/↓ or Tab: navigate   Enter: run   I/W/Q: hotkeys   Esc: quit"),
	)
}

func (m tuiModel) workerMenuRows(running bool) []string {
	rows := []string{}
	for i, a := range m.actions {
		if !m.actionAllowed(a.Key) {
			continue
		}
		label := a.Label
		if a.Key == "w" {
			if running {
				label = "Stop worker"
			} else {
				label = "Start worker"
			}
		}
		row := fmt.Sprintf("❯ [%s] %s", strings.ToUpper(a.Key), label)
		if i == m.selected {
			rows = append(rows, m.styles.selected.Render(padRight(row, 52)))
		} else {
			rows = append(rows, "  "+m.styles.muted.Render(fmt.Sprintf("[%s]", strings.ToUpper(a.Key)))+" "+label)
		}
	}
	return rows
}

func (m tuiModel) actionLabel(a tuiAction, snap map[string]any) string {
	label := a.Label
	switch a.Key {
	case "c":
		if running, _ := snap["coordinatorRunning"].(bool); running {
			label = "Stop coordinator"
		} else {
			label = "Start coordinator"
		}
	case "w":
		if running, _ := snap["workerRunning"].(bool); running {
			label = "Stop worker"
		} else {
			label = "Start worker"
		}
	}
	return label
}

func (m tuiModel) coordinatorStatusBadge(snap map[string]any) string {
	running, _ := snap["coordinatorRunning"].(bool)
	if !running {
		return m.styles.muted.Render("○ OFFLINE")
	}
	status := strings.ToLower(safeString(snap["modelStatus"]))
	switch status {
	case "ready":
		return m.styles.ok.Render("● READY")
	case "loading":
		return m.styles.warn.Render("◐ LOADING")
	case "processing":
		return m.styles.warn.Render("● PROCESSING")
	case "unreachable":
		return m.styles.bad.Render("● UNREACHABLE")
	case "error":
		return m.styles.bad.Render("● ERROR")
	default:
		return m.styles.warn.Render("● RUNNING")
	}
}

func (m tuiModel) clusterCapacityPanel(width int, snap map[string]any, cfg Config) string {
	hw := hardwareInfo()
	workers, _ := snap["workerStatuses"].([]Worker)
	totalRAM := hw.RAMBytes
	totalVRAM := hw.VRAMBytes
	usable := workerUsableGB(Worker{OS: runtime.GOOS, Arch: runtime.GOARCH, RAMBytes: hw.RAMBytes, VRAMBytes: hw.VRAMBytes, Backend: hw.Backend})
	connected := 0
	for _, w := range workers {
		if w.OK || strings.Contains(strings.ToLower(w.Status), "connected") || strings.Contains(strings.ToLower(w.Status), "busy") {
			connected++
			totalRAM += w.RAMBytes
			totalVRAM += w.VRAMBytes
			usable += workerUsableGB(w)
		}
	}
	rows := []string{
		fmt.Sprintf("nodes %d total (%d connected workers) • local %s • %s", len(workers)+1, connected, runtime.GOOS, hw.Backend),
		fmt.Sprintf("RAM %s total • VRAM %s total • usable estimate %.1fG", humanBytes(totalRAM), humanBytes(totalVRAM), usable),
		fmt.Sprintf("workers configured %d • discover with [D]", len(workers)),
	}
	return m.box("Cluster capacity", width, rows)
}

func (m tuiModel) launchSettingsPanel(width int, cfg Config) string {
	mode := "manual"
	model := "none"
	if strings.TrimSpace(cfg.ModelPath) != "" {
		model = filepath.Base(cfg.ModelPath)
	}
	appPort := m.state.appPort
	if appPort == 0 {
		appPort = 8765
	}
	rows := []string{
		fmt.Sprintf("OpenAI API http://127.0.0.1:%d/v1", appPort),
		fmt.Sprintf("model %s • [M] select existing • [B] browse/download", model),
		fmt.Sprintf("mode %s • compute %s • memory %s", mode, cfg.ComputeMode, memoryMode(cfg)),
		fmt.Sprintf("GPU/RPC layers %d • layer plan %s • threads %d", cfg.GPULayers, layerPlanSummary(cfg, cfg.Workers), cfg.Threads),
		fmt.Sprintf("context %d • batch %d • ubatch %d • parallel %d • cache-ram %d", cfg.Context, cfg.Batch, cfg.UBatch, cfg.Parallel, cfg.CacheRAM),
		"minimal: set compute CPU or gpu-layers 0; ClusterKit will not auto-overrule it",
		"coordinator compute off: local machine hosts API only; RPC workers get layer weights",
		"start: [C] Start coordinator uses these settings; logs open in separate Terminal",
	}
	if cfg.GPULayers == 0 && len(cfg.Workers) > 0 {
		rows = append(rows, m.styles.warn.Render("workers will stay mostly idle: GPU/RPC layers is 0. Set it above 0 to offload model layers to RPC workers."))
	}
	if m.coordinatorRunning() {
		rows = append(rows, m.styles.warn.Render("locked: stop coordinator to edit model/launch settings"))
	}
	return m.box("Launch settings", width, rows)
}

func (m tuiModel) modelBrowserView(width int) string {
	s := m.styles
	var rows []string
	title := "Models"
	if m.busy {
		return lipgloss.JoinVertical(lipgloss.Left,
			m.box("Models", width, []string{s.warn.Render(m.busyText), s.muted.Render("input locked until operation finishes")}),
		)
	}
	switch m.browser {
	case "files":
		title = "Model browser — files in " + m.hfRepo
		if len(m.hfFiles) == 0 {
			rows = append(rows, s.muted.Render("no GGUF files found"))
		}
		visible := max(4, m.height-10)
		m.fileOffset = adjustScroll(m.fileSel, m.fileOffset, visible, len(m.hfFiles))
		end := min(len(m.hfFiles), m.fileOffset+visible)
		for i := m.fileOffset; i < end; i++ {
			f := m.hfFiles[i]
			row := fmt.Sprintf("%-62s %8s", short(f.RFilename, 62), humanBytes(uint64(f.Size)))
			if i == m.fileSel {
				row = s.selected.Render(padRight("❯ "+row, width-8))
			} else {
				row = "  " + row
			}
			rows = append(rows, row)
		}
		rows = append(rows, "", s.muted.Render(fmt.Sprintf("%d/%d  Enter: download selected  ←: repos  Esc/Q: back", min(m.fileSel+1, len(m.hfFiles)), len(m.hfFiles))))
	case "repos":
		title = "Model browser — Hugging Face"
		searchLine := "Search: " + m.hfSearchIn.View()
		if !m.hfSearchFocus {
			searchLine = "Search: " + defaultHFQuery(m.hfQuery) + "    " + s.muted.Render("press / to edit")
		}
		rows = append(rows, searchLine, "")
		if len(m.hfModels) == 0 {
			rows = append(rows, s.muted.Render("press R to search qwen gguf"))
		}
		visible := max(4, m.height-12)
		m.hfOffset = adjustScroll(m.hfSel, m.hfOffset, visible, len(m.hfModels))
		end := min(len(m.hfModels), m.hfOffset+visible)
		for i := m.hfOffset; i < end; i++ {
			repo := m.hfModels[i]
			row := fmt.Sprintf("%-54s downloads %-8d likes %d", short(repo.ID, 54), repo.Downloads, repo.Likes)
			if i == m.hfSel {
				row = s.selected.Render(padRight("❯ "+row, width-8))
			} else {
				row = "  " + row
			}
			rows = append(rows, row)
		}
		rows = append(rows, "", s.muted.Render(fmt.Sprintf("%d/%d  /: search  Enter/→: open files  Tab: local models  R: refresh  Esc/Q: back", min(m.hfSel+1, len(m.hfModels)), len(m.hfModels))))
	default:
		title = "Select local model"
		models := m.state.localModels()
		if len(models) == 0 {
			rows = append(rows, s.muted.Render("no local GGUF models yet — Tab to browse/download"))
		}
		visible := max(4, m.height-10)
		m.localOffset = adjustScroll(m.localSel, m.localOffset, visible, len(models))
		end := min(len(models), m.localOffset+visible)
		for i := m.localOffset; i < end; i++ {
			model := models[i]
			mark := " "
			if model.Selected {
				mark = "✓"
			}
			row := fmt.Sprintf("%s %-56s %8s ctx %d", mark, short(model.Name, 56), humanBytes(uint64(model.Size)), model.MaxContext)
			if i == m.localSel {
				row = s.selected.Render(padRight("❯ "+row, width-8))
			} else {
				row = "  " + row
			}
			rows = append(rows, row)
		}
		rows = append(rows, "", s.muted.Render(fmt.Sprintf("%d/%d  Enter: select  Tab: browse/download  Esc/Q: back", min(m.localSel+1, len(models)), len(models))))
	}
	return m.box(title, width, rows)
}

func (m tuiModel) apiView(width int) string {
	s := m.styles
	m.state.mu.Lock()
	cfg := m.state.config
	appPort := m.state.appPort
	coordinatorRunning := m.state.serverCmd != nil && m.state.serverCmd.Process != nil
	serverLoad := m.state.serverLoad
	m.state.mu.Unlock()
	if appPort == 0 {
		appPort = 8765
	}
	lanIP := localIP()
	if lanIP == "" {
		lanIP = "<this-machine-lan-ip>"
	}
	localBase := fmt.Sprintf("http://127.0.0.1:%d/v1", appPort)
	lanBase := fmt.Sprintf("http://%s:%d/v1", lanIP, appPort)
	localRoot := fmt.Sprintf("http://127.0.0.1:%d", appPort)
	lanRoot := fmt.Sprintf("http://%s:%d", lanIP, appPort)
	coordBase := fmt.Sprintf("http://127.0.0.1:%d/v1", cfg.APIPort)
	coordReachable := coordinatorPortReachable(cfg.APIPort)
	status := "unavailable"
	if coordinatorRunning || coordReachable {
		status = "ok"
	}
	model := "clusterkit-local"
	if strings.TrimSpace(cfg.ModelPath) != "" {
		model = filepath.Base(cfg.ModelPath)
	}
	rows := []string{
		s.muted.Render("Esc/Q: back   R: refresh"),
		fmt.Sprintf("status %-12s coordinator process %-5s coordinator port %-5s", status, onOff(coordinatorRunning), onOff(coordReachable)),
		fmt.Sprintf("model  %s", model),
		fmt.Sprintf("load   %s", firstNonEmpty(serverLoad, "unknown")),
		"",
		"Use on this machine:",
		fmt.Sprintf("  base_url    %s", localBase),
		fmt.Sprintf("  health      %s/health", localRoot),
		fmt.Sprintf("  models      %s/models", localBase),
		fmt.Sprintf("  chat        %s/chat/completions", localBase),
		fmt.Sprintf("  completions %s/completions", localBase),
		"",
		"Use from another device on the same LAN:",
		fmt.Sprintf("  base_url    %s", lanBase),
		fmt.Sprintf("  health      %s/health", lanRoot),
		fmt.Sprintf("  models      %s/models", lanBase),
		fmt.Sprintf("  chat        %s/chat/completions", lanBase),
		fmt.Sprintf("  completions %s/completions", lanBase),
		"",
		"Compatibility aliases if a client wants server root instead of /v1:",
		fmt.Sprintf("  root        %s", localRoot),
		fmt.Sprintf("  LAN root    %s", lanRoot),
		"  aliases     /models  /chat/completions  /completions",
		"",
		"Upstream coordinator (internal llama-server):",
		fmt.Sprintf("  %s", coordBase),
	}
	if status != "ok" {
		rows = append(rows, "", s.warn.Render("OpenAI endpoints are visible now, but generation needs [C] Start coordinator first."))
	}
	rows = append(rows, "", s.muted.Render("For OpenAI SDK: api_key can be any non-empty string; model can be clusterkit-local or the listed model id."))
	return m.box("API endpoints", width, rows)
}

func (m tuiModel) settingsView(width int) string {
	s := m.styles
	if m.coordinatorRunning() {
		return m.box("Launch settings locked", width, []string{
			s.warn.Render("Coordinator is running."),
			"Stop coordinator before changing model/context/batch/compute settings.",
			s.muted.Render("Esc/Q: back"),
		})
	}
	m.state.mu.Lock()
	cfg := m.state.config
	m.state.mu.Unlock()
	maxCtx := 32768
	if strings.TrimSpace(cfg.ModelPath) != "" {
		if v, err := ggufMaxContext(cfg.ModelPath); err == nil && v > 0 {
			maxCtx = v
		}
	}
	vals := map[string]string{
		"model":            "none",
		"context":          fmt.Sprintf("%d / max %d", cfg.Context, maxCtx),
		"batch":            fmt.Sprintf("%d", cfg.Batch),
		"ubatch":           fmt.Sprintf("%d", cfg.UBatch),
		"parallel":         fmt.Sprintf("%d", cfg.Parallel),
		"threads":          fmt.Sprintf("%d", cfg.Threads),
		"gpuLayers":        fmt.Sprintf("%d", cfg.GPULayers),
		"cacheRam":         fmt.Sprintf("%d MiB", cfg.CacheRAM),
		"chatTimeout":      fmt.Sprintf("%d sec", chatTimeoutSec(cfg)),
		"chatMaxTokens":    fmt.Sprintf("%d", defaultChatMaxTokens(cfg)),
		"chatNoTokenLimit": yesNo(cfg.ChatNoTokenLimit),
		"showThinking":     onOff(!cfg.HideThinking),
		"splitMode":        splitMode(cfg),
		"tensorSplit":      displayTensorSplit(cfg),
		"computeMode":      cfg.ComputeMode,
		"memoryMode":       memoryMode(cfg),
		"coordinatorLocal": yesNo(cfg.CoordinatorLocal),
	}
	if strings.TrimSpace(cfg.ModelPath) != "" {
		vals["model"] = filepath.Base(cfg.ModelPath)
	}
	labels := map[string]string{
		"model":            "Model",
		"context":          "Context window",
		"batch":            "Batch size",
		"ubatch":           "Micro batch",
		"parallel":         "Parallel slots",
		"threads":          "CPU threads",
		"gpuLayers":        "GPU/RPC layers",
		"cacheRam":         "KV cache RAM",
		"chatTimeout":      "Chat timeout",
		"chatMaxTokens":    "Max output tokens",
		"chatNoTokenLimit": "No token limit",
		"showThinking":     "Show model thinking",
		"splitMode":        "Split mode",
		"tensorSplit":      "Tensor split",
		"computeMode":      "Compute mode",
		"memoryMode":       "Memory mode",
		"coordinatorLocal": "Coordinator compute",
	}
	rows := []string{s.muted.Render("↑/↓ select   ←/→ change   Shift+←/→ fine adjust context/tokens   Esc back")}
	for i, key := range launchSettingKeys() {
		row := fmt.Sprintf("%-20s • %s", labels[key], vals[key])
		if i == m.settingSel {
			row = s.selected.Render(padRight("❯ "+row, width-8))
		} else {
			row = "  " + row
		}
		rows = append(rows, row)
	}
	rows = append(rows, "", s.muted.Render("Tensor split: ←/→ cycles auto/equal/usable/layer-plan. For exact per-worker layer tests use [L]."))
	return m.box("Launch settings", width, rows)
}

func (m tuiModel) workerLayersView(width int) string {
	s := m.styles
	if m.coordinatorRunning() {
		return m.box("Worker layers locked", width, []string{
			s.warn.Render("Coordinator is running."),
			"Stop coordinator before changing per-worker layer routing.",
			s.muted.Render("Esc/Q: back"),
		})
	}
	m.state.mu.Lock()
	cfg := m.state.config
	m.state.mu.Unlock()
	workers := cfg.Workers
	if m.workerLayerSel >= len(workers)+1 {
		m.workerLayerSel = len(workers)
	}
	rows := []string{
		s.muted.Render("↑/↓ select   Ctrl+↑/↓ move RPC workers only   ←/→ layers ±1   Shift+←/→ ±4   0 zero   E enable/disable worker   A auto   R reset   Esc back"),
		fmt.Sprintf("RPC order %s • plan %s • total %d • config GPU/RPC layers %d", workerOrderSummary(workers), layerPlanSummary(cfg, workers), manualLayerTotal(cfg, workers), cfg.GPULayers),
		"",
	}
	localLine := fmt.Sprintf("LOCAL %-18s %-15s %-10s layers %-3d enable %-3s RAM %7s VRAM %7s", "coordinator", "127.0.0.1", "LOCAL", cfg.CoordinatorLayers, yesNo(cfg.CoordinatorLayers > 0), humanBytes(hardwareInfo().RAMBytes), humanBytes(hardwareInfo().VRAMBytes))
	if m.workerLayerSel == 0 {
		localLine = s.selected.Render(padRight("❯ "+localLine, width-8))
	} else if cfg.CoordinatorLayers == 0 {
		localLine = "  " + s.muted.Render(localLine)
	} else {
		localLine = "  " + localLine
	}
	rows = append(rows, localLine)
	for i, w := range workers {
		status := strings.ToUpper(firstNonEmpty(w.Status, "unknown"))
		enabled := "on"
		if w.Disabled {
			enabled = "off"
		}
		line := fmt.Sprintf("RPC%-2d %-18s %-15s %s layers %-3d enable %-3s RAM %7s VRAM %7s crash %d", i, short(firstNonEmpty(w.Name, w.Host), 18), short(w.Host, 15), short(status, 10), w.Layers, enabled, humanBytes(w.RAMBytes), humanBytes(w.VRAMBytes), w.CrashCount)
		rowIndex := i + 1
		if rowIndex == m.workerLayerSel {
			line = s.selected.Render(padRight("❯ "+line, width-8))
		} else if w.Disabled || w.Layers == 0 {
			line = "  " + s.muted.Render(line)
		} else {
			line = "  " + line
		}
		rows = append(rows, line)
	}
	rows = append(rows, "", s.muted.Render("LOCAL is fixed local compute, not part of RPC order. Active workers below are RPC0, RPC1… Tensor-split follows RPC order after any LOCAL value."))
	return m.box("Worker layers", width, rows)
}

func (m tuiModel) chatView(width int) string {
	s := m.styles
	m.state.mu.Lock()
	cfg := m.state.config
	ctx := m.state.config.Context
	runningCtx := m.state.serverContext
	m.state.mu.Unlock()
	ctxLabel := fmt.Sprintf("%d", ctx)
	if runningCtx > 0 {
		ctx = runningCtx
		ctxLabel = fmt.Sprintf("%d running", runningCtx)
		if cfg.Context != runningCtx {
			ctxLabel = fmt.Sprintf("%d running • config %d", runningCtx, cfg.Context)
		}
	}
	inner := max(20, width-4)
	textWidth := max(24, inner)
	rows := []string{}
	if len(m.chat) == 0 {
		rows = append(rows, s.muted.Render("No messages yet. Type below and press Enter."))
	}
	for _, line := range m.chat {
		role := line.Role
		style := s.muted
		switch role {
		case "user":
			style = s.title
		case "assistant":
			style = s.ok
		case "error":
			style = s.bad
		}
		meta := chatLineMeta(line)
		for i, wrapped := range wrapVisible(role+": "+line.Text+meta, textWidth) {
			if i == 0 {
				rows = append(rows, style.Render(wrapped))
			} else {
				rows = append(rows, style.Render(wrapped))
			}
		}
	}
	maxRows := max(4, m.height-12)
	if len(rows) > maxRows {
		rows = rows[len(rows)-maxRows:]
	}
	if m.busy {
		rows = append(rows, "", s.warn.Render(m.busyText))
	}
	chatBox := m.box("Chat", width, rows)
	m.chatIn.SetWidth(max(20, inner-2))
	m.chatIn.SetHeight(2)
	model := "none"
	if cfg.ModelPath != "" {
		model = filepath.Base(cfg.ModelPath)
	}
	contextTokens := estimateChatContextTokens(m.chat, m.chatIn.Value())
	input := m.box("Prompt", width, []string{
		m.chatIn.View(),
		s.muted.Render("Enter: send   Ctrl+K: clear chat/context   Esc: back"),
		s.muted.Render(fmt.Sprintf("model: %s • context: %s • prompt ctx: ~%d / %d tok • last total: %d • thinking: %s", short(model, max(16, inner-62)), ctxLabel, contextTokens, ctx, m.chatUsed, onOff(m.showThinking()))),
	})
	return lipgloss.JoinVertical(lipgloss.Left, chatBox, input)
}

func estimateChatContextTokens(lines []chatLine, draft string) int {
	estimateLines := make([]chatLine, 0, len(lines)+1)
	estimateLines = append(estimateLines, lines...)
	if strings.TrimSpace(draft) != "" {
		estimateLines = append(estimateLines, chatLine{Role: "user", Text: draft})
	}
	messages := chatMessagesForModel(estimateLines)
	tokens := 0
	for _, msg := range messages {
		// Chat templates add role/control tokens around every message. This is an
		// intentionally conservative live estimate for the TUI footer; exact counts
		// require model-specific tokenization after the request reaches llama.cpp.
		tokens += 6
		tokens += estimateTextTokens(msg["role"])
		tokens += estimateTextTokens(msg["content"])
	}
	// Assistant generation prefix / final template overhead.
	return tokens + 4
}

func estimateTextTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	runes := []rune(text)
	words := strings.Fields(text)
	// Mixed-language approximation: Latin text is usually ~4 chars/token,
	// Cyrillic/CJK and punctuation-heavy text are closer to rune-based counts.
	byChars := (len(runes) + 3) / 4
	byWords := int(float64(len(words))*1.35) + 1
	if byWords > byChars {
		return byWords
	}
	return byChars
}

func chatMessagesForModel(lines []chatLine) []map[string]string {
	messages := make([]map[string]string, 0, len(lines)+1)
	messages = append(messages, map[string]string{
		"role":    "system",
		"content": "You are a helpful local chat assistant. Answer in the user's language. If you use hidden reasoning, keep it brief, finish it, then provide the final answer. Do not continue indefinitely, do not repeat zeros, and stop when the answer is complete.",
	})
	for _, line := range lines {
		role := line.Role
		switch role {
		case "user", "assistant":
			// valid OpenAI-compatible chat roles
		default:
			continue
		}
		text := strings.TrimSpace(line.Text)
		if text == "" {
			continue
		}
		messages = append(messages, map[string]string{"role": role, "content": text})
	}
	return messages
}

func chatLineMeta(line chatLine) string {
	parts := []string{}
	if !line.At.IsZero() {
		parts = append(parts, line.At.Format("15:04"))
	}
	if line.Ms > 0 {
		parts = append(parts, fmt.Sprintf("%.1fs", float64(line.Ms)/1000))
	}
	if line.Tokens > 0 {
		parts = append(parts, fmt.Sprintf("%d tok", line.Tokens))
		if line.Ms > 0 {
			sec := float64(line.Ms) / 1000
			if sec > 0 {
				parts = append(parts, fmt.Sprintf("%.1f tok/s", float64(line.Tokens)/sec))
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "  • " + strings.Join(parts, " • ")
}

func wrapVisible(text string, width int) []string {
	width = max(1, width)
	out := []string{}
	for _, paragraph := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range words {
			if line == "" {
				for lipgloss.Width(word) > width {
					part, rest := splitVisible(word, width)
					out = append(out, part)
					word = rest
				}
				line = word
				continue
			}
			if lipgloss.Width(line+" "+word) <= width {
				line += " " + word
				continue
			}
			out = append(out, line)
			line = ""
			for lipgloss.Width(word) > width {
				part, rest := splitVisible(word, width)
				out = append(out, part)
				word = rest
			}
			line = word
		}
		if line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func splitVisible(s string, width int) (string, string) {
	if width <= 0 || s == "" {
		return "", s
	}
	out := ""
	for i, r := range s {
		candidate := out + string(r)
		if out != "" && lipgloss.Width(candidate) > width {
			return out, s[i:]
		}
		out = candidate
	}
	return out, ""
}

func (m tuiModel) loadingBox(width int) string {
	dots := strings.Repeat(".", int(time.Now().UnixMilli()/300)%4)
	text := m.busyText
	if text == "" {
		text = "Working"
	}
	return m.box("Working", width, []string{
		m.styles.warn.Render(text + dots),
		m.styles.muted.Render("input is locked until this operation finishes"),
	})
}

func asciiTUI() bool {
	return runtime.GOOS == "windows" || os.Getenv("CLUSTERKIT_ASCII") == "1"
}

func thinkingHeader() string {
	if asciiTUI() {
		return "Thinking:"
	}
	return "🧠 Thinking:"
}

func terminalText(s string) string {
	if !asciiTUI() || s == "" {
		return s
	}
	replacer := strings.NewReplacer(
		"🧠 ", "",
		"🧠", "",
		"❯", ">",
		"•", "-",
		"↑", "Up",
		"↓", "Down",
		"←", "Left",
		"→", "Right",
		"±", "+/-",
		"…", "...",
		"—", "-",
		"–", "-",
		"‑", "-",
		"Wi‑Fi", "Wi-Fi",
		"●", "*",
		"○", "o",
		"◐", "~",
		"✓", "x",
	)
	return replacer.Replace(s)
}

func (m tuiModel) box(title string, width int, rows []string) string {
	inner := max(20, width-4)
	clean := make([]string, 0, len(rows)+1)
	clean = append(clean, m.styles.title.Render(terminalText(title)))
	for _, row := range rows {
		clean = append(clean, clipVisible(terminalText(row), inner))
	}
	return m.styles.box.Width(width).Render(lipgloss.JoinVertical(lipgloss.Left, clean...))
}

func (m tuiModel) statusStyle(status string) lipgloss.Style {
	s := strings.ToLower(status)
	switch {
	case strings.Contains(s, "connected"), strings.Contains(s, "ready"), strings.Contains(s, "ok"):
		return m.styles.ok
	case strings.Contains(s, "busy"), strings.Contains(s, "agent"), strings.Contains(s, "discover"):
		return m.styles.warn
	case strings.Contains(s, "offline"), strings.Contains(s, "fail"), strings.Contains(s, "crash"):
		return m.styles.bad
	default:
		return m.styles.base
	}
}

func (m tuiModel) modelStatusBadge(status string) string {
	s := strings.ToLower(strings.TrimSpace(status))
	if s == "" {
		s = "offline"
	}
	label := strings.ToUpper(s)
	switch s {
	case "ready":
		return m.styles.ok.Render("● " + label)
	case "loading":
		return m.styles.warn.Render("◐ " + label)
	case "processing":
		return m.styles.warn.Render("● " + label)
	case "unreachable":
		return m.styles.bad.Render("● " + label)
	case "offline":
		return m.styles.muted.Render("○ " + label)
	case "error":
		return m.styles.bad.Render("● " + label)
	default:
		return m.styles.muted.Render("• " + label)
	}
}

func padRight(s string, n int) string {
	s = terminalText(s)
	if lipgloss.Width(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-lipgloss.Width(s))
}

func clipVisible(s string, n int) string {
	ellipsis := ellipsis()
	if lipgloss.Width(s) <= n {
		return s
	}
	runes := []rune(s)
	out := ""
	for _, r := range runes {
		if lipgloss.Width(out+string(r)+ellipsis) > n {
			break
		}
		out += string(r)
	}
	return out + ellipsis
}

func ellipsis() string {
	if asciiTUI() {
		return "..."
	}
	return "…"
}

func dash() string {
	if asciiTUI() {
		return "-"
	}
	return "—"
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func yesNo(v bool) string {
	if v {
		return "ready"
	}
	return "not ready"
}

func safeString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func portOnly(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return addr
}

func statusANSI(status string) string {
	s := strings.ToLower(status)
	switch {
	case strings.Contains(s, "connected"), strings.Contains(s, "ready"), strings.Contains(s, "ok"):
		return "\033[32m"
	case strings.Contains(s, "busy"), strings.Contains(s, "agent"), strings.Contains(s, "discover"):
		return "\033[33m"
	case strings.Contains(s, "offline"), strings.Contains(s, "fail"), strings.Contains(s, "crash"):
		return "\033[31m"
	default:
		return "\033[37m"
	}
}

func short(s string, n int) string {
	if s == "" {
		return "unknown"
	}
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + ellipsis()
}

func humanBytes(v uint64) string {
	if v == 0 {
		return dash()
	}
	gb := float64(v) / 1024 / 1024 / 1024
	if gb >= 1 {
		return fmt.Sprintf("%.1fG", gb)
	}
	mb := float64(v) / 1024 / 1024
	return fmt.Sprintf("%.0fM", mb)
}

func (s *AppState) autostartWorker() {
	s.mu.Lock()
	cfg := s.config
	running := s.workerCmd != nil
	manualStopped := s.workerManualStopped
	s.mu.Unlock()
	if !cfg.RoleExplicit || cfg.Role != "worker" || running || manualStopped || !fileExists(llamaBin(cfg.LlamaDir, "rpc-server")) {
		return
	}
	s.addLog("worker autostart")
	s.startWorkerProcess(cfg)
}

func defaultConfig() Config {
	return Config{
		Role:             "",
		RoleExplicit:     false,
		APIPort:          8080,
		RPCPort:          50052,
		LlamaDir:         defaultLlamaDir(),
		ModelsDir:        defaultModelsDir(),
		Context:          4096,
		GPULayers:        0,
		Threads:          max(1, runtime.NumCPU()-1),
		Parallel:         1,
		CacheRAM:         0,
		ChatTimeout:      1800,
		Batch:            64,
		UBatch:           32,
		SplitMode:        "layer",
		ComputeMode:      "auto",
		MemoryMode:       "mmap",
		CoordinatorLocal: true,
		Workers:          []Worker{},
	}
}

func resetStartupRole(cfg Config) Config {
	// Role is intentionally session-scoped. Persisting coordinator/worker caused
	// surprising side effects on the next launch: worker probing, discovery
	// announcements, and autostart before the user picked this machine's role.
	cfg.Role = ""
	cfg.RoleExplicit = false
	return cfg
}

func localListener(preferredPort int) (net.Listener, string, error) {
	for _, port := range []int{preferredPort, 0} {
		addr := "0.0.0.0:" + strconv.Itoa(port)
		listener, err := net.Listen("tcp", addr)
		if err == nil {
			_, portText, _ := net.SplitHostPort(listener.Addr().String())
			return listener, "127.0.0.1:" + portText, nil
		}
		if port == 0 {
			return nil, "", err
		}
	}
	return nil, "", errors.New("failed to bind local UI port")
}

func firstFreePort(preferred int) (int, bool) {
	if preferred <= 0 {
		preferred = 8080
	}
	for port := preferred; port < preferred+50; port++ {
		ln, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(port))
		if err == nil {
			_ = ln.Close()
			return port, port != preferred
		}
	}
	return preferred, false
}

func appDir() string {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, "ClusterKit")
		}
	}
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "ClusterKit")
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".clusterkit")
	}
	return ".clusterkit"
}

func configPath() string       { return filepath.Join(appDir(), "config.json") }
func defaultLlamaDir() string  { return filepath.Join(appDir(), "llama.cpp") }
func defaultModelsDir() string { return filepath.Join(appDir(), "models") }
func modelsDir(cfg Config) string {
	if cfg.ModelsDir != "" {
		return cfg.ModelsDir
	}
	return defaultModelsDir()
}
func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func loadConfig() (Config, error) {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	return cfg, json.Unmarshal(b, &cfg)
}

func saveConfig(cfg Config) error {
	if cfg.APIPort == 0 {
		cfg.APIPort = 8080
	}
	if !cfg.RoleExplicit {
		cfg.Role = ""
	}
	if cfg.RPCPort == 0 {
		cfg.RPCPort = 50052
	}
	if cfg.Context == 0 {
		cfg.Context = 4096
	}
	if cfg.Threads == 0 {
		cfg.Threads = max(1, runtime.NumCPU()-1)
	}
	if cfg.Parallel == 0 {
		cfg.Parallel = 1
	}
	if cfg.Batch == 0 {
		cfg.Batch = 512
	}
	if cfg.UBatch == 0 {
		cfg.UBatch = 512
	}
	if cfg.ChatTimeout == 0 {
		cfg.ChatTimeout = 1800
	}
	if cfg.ComputeMode == "" {
		cfg.ComputeMode = "auto"
	}
	if cfg.MemoryMode == "" {
		cfg.MemoryMode = "normal"
	}
	cfg.SplitMode = splitMode(cfg)
	// Legacy fields kept in Config only for old config.json compatibility. They
	// must not affect launches: user-entered gpuLayers/batch/ubatch are strict.
	cfg.WorkerPolicy = ""
	cfg.WeightedMode = false
	if cfg.LlamaDir == "" {
		cfg.LlamaDir = defaultLlamaDir()
	}
	if cfg.ModelsDir == "" {
		cfg.ModelsDir = defaultModelsDir()
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(configPath(), b, 0644)
}

func chatTimeoutSec(cfg Config) int {
	sec := cfg.ChatTimeout
	if sec <= 0 {
		sec = 1800
	}
	return clampInt(sec, 30, 86400)
}

func chatTimeoutDuration(cfg Config) time.Duration {
	return time.Duration(chatTimeoutSec(cfg)) * time.Second
}

func (s *AppState) addLog(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	s.logs = append(s.logs, line)
	if len(s.logs) > 200 {
		s.logs = s.logs[len(s.logs)-200:]
	}
	appendAppLog(line)
}

func appendAppLog(line string) {
	_ = os.MkdirAll(logsDir(), 0755)
	f, err := os.OpenFile(filepath.Join(logsDir(), "clusterkit-ui.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintln(f, line)
}

func (s *AppState) modelStatusLocked() string {
	if s.serverCmd == nil || s.serverCmd.Process == nil {
		return "offline"
	}
	return statusFromServerLoad(s.serverLoad)
}

func statusFromServerLoad(loadText string) string {
	load := strings.ToLower(strings.TrimSpace(loadText))
	switch {
	case load == "", load == "starting", strings.Contains(load, "loading"), strings.Contains(load, "warming"), strings.Contains(load, "initializing"):
		return "loading"
	case strings.Contains(load, "processing"):
		return "processing"
	case strings.Contains(load, "ready"):
		return "ready"
	case strings.Contains(load, "error"), strings.Contains(load, "oom"):
		return "error"
	default:
		return load
	}
}

func effectiveCoordinatorStatus(running bool, serverLoad, apiHealth string) string {
	if !running {
		return "offline"
	}
	loadStatus := statusFromServerLoad(serverLoad)
	switch apiHealth {
	case "ready":
		if loadStatus == "processing" {
			return "processing"
		}
		return "ready"
	case "loading":
		return "loading"
	case "unreachable":
		if loadStatus == "loading" {
			return "loading"
		}
		return "unreachable"
	default:
		return loadStatus
	}
}

func coordinatorAPIHealth(port int, running bool) string {
	if !running || port <= 0 {
		return "offline"
	}
	client := &http.Client{Timeout: 120 * time.Millisecond}
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	resp, err := client.Get(base + "/health")
	if err != nil {
		return "unreachable"
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "ready"
	}
	if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooEarly || resp.StatusCode == http.StatusLocked {
		return "loading"
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		resp, err = client.Get(base + "/v1/models")
		if err != nil {
			return "unreachable"
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return "ready"
		}
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooEarly || resp.StatusCode == http.StatusLocked {
			return "loading"
		}
	}
	return "unreachable"
}

func (s *AppState) snapshot() map[string]any {
	s.mu.Lock()
	cfg := s.config
	workerRunning := s.workerCmd != nil && s.workerCmd.Process != nil
	coordinatorRunning := s.serverCmd != nil && s.serverCmd.Process != nil
	serverLoad := s.serverLoad
	serverContext := s.serverContext
	serverRPC := s.serverRPC
	loadStarted := s.loadStarted
	loadReady := s.loadReady
	download := s.download
	logs := append([]string(nil), s.logs...)
	s.mu.Unlock()
	workerStatuses := append([]Worker(nil), cfg.Workers...)
	if isCoordinatorRole(cfg) {
		workerStatuses = s.workerStatusesFor(cfg)
	}
	modelStatus := effectiveCoordinatorStatus(coordinatorRunning, serverLoad, coordinatorAPIHealth(cfg.APIPort, coordinatorRunning))
	return map[string]any{
		"config":             cfg,
		"workerStatuses":     workerStatuses,
		"localIP":            localIP(),
		"os":                 runtime.GOOS,
		"arch":               runtime.GOARCH,
		"workerRunning":      workerRunning,
		"coordinatorRunning": coordinatorRunning,
		"serverLoad":         serverLoad,
		"serverContext":      serverContext,
		"serverRPC":          serverRPC,
		"modelStatus":        modelStatus,
		"loadStartedMs":      millisSince(loadStarted),
		"loadReadyMs":        millisSince(loadReady),
		"logs":               logs,
		"appDir":             appDir(),
		"hardware":           hardwareInfo(),
		"modelsDir":          modelsDir(cfg),
		"llamaReady":         installStatus(cfg).Ready,
		"installStatus":      installStatus(cfg),
		"download":           download,
	}
}

func (s *AppState) fastSnapshot() map[string]any {
	s.mu.Lock()
	cfg := s.config
	workers := append([]Worker(nil), cfg.Workers...)
	logs := append([]string(nil), s.logs...)
	workerRunning := s.workerCmd != nil && s.workerCmd.Process != nil
	coordinatorRunning := s.serverCmd != nil && s.serverCmd.Process != nil
	serverLoad := s.serverLoad
	serverContext := s.serverContext
	serverRPC := s.serverRPC
	loadStarted := s.loadStarted
	loadReady := s.loadReady
	download := s.download
	s.mu.Unlock()
	modelStatus := effectiveCoordinatorStatus(coordinatorRunning, serverLoad, coordinatorAPIHealth(cfg.APIPort, coordinatorRunning))
	return map[string]any{
		"config":             cfg,
		"workerStatuses":     workers,
		"localIP":            localIP(),
		"os":                 runtime.GOOS,
		"arch":               runtime.GOARCH,
		"workerRunning":      workerRunning,
		"coordinatorRunning": coordinatorRunning,
		"serverLoad":         serverLoad,
		"serverContext":      serverContext,
		"serverRPC":          serverRPC,
		"modelStatus":        modelStatus,
		"loadStartedMs":      millisSince(loadStarted),
		"loadReadyMs":        millisSince(loadReady),
		"logs":               logs,
		"appDir":             appDir(),
		"hardware":           HardwareInfo{},
		"modelsDir":          modelsDir(cfg),
		"llamaReady":         fileExists(llamaBin(cfg.LlamaDir, "llama-server")) && fileExists(llamaBin(cfg.LlamaDir, "rpc-server")),
		"installStatus":      InstallStatus{},
		"download":           download,
	}
}

func (s *AppState) localModels() []tuiLocalModel {
	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()
	root := modelsDir(cfg)
	_ = os.MkdirAll(root, 0755)
	items := []tuiLocalModel{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".gguf") {
			if st, e := d.Info(); e == nil {
				maxCtx, _ := ggufMaxContext(path)
				items = append(items, tuiLocalModel{Name: d.Name(), Path: path, Size: st.Size(), Selected: path == cfg.ModelPath, Aux: isAuxModelFile(path), MaxContext: maxCtx})
			}
		}
		return nil
	})
	sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name) })
	return items
}

func (s *AppState) selectModelPath(path string) {
	if isAuxModelFile(path) {
		s.addLog("cannot select auxiliary model file: %s", path)
		return
	}
	s.mu.Lock()
	cfg := s.config
	cfg.ModelPath = path
	s.config = cfg
	s.mu.Unlock()
	_ = saveConfig(cfg)
	s.addLog("selected model: %s", path)
}

func (s *AppState) handleStatus(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.snapshot()) }

func (s *AppState) handleConfig(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.config) }

func (s *AppState) handleSave(w http.ResponseWriter, r *http.Request) {
	var cfg Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := saveConfig(cfg); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.mu.Lock()
	s.config = cfg
	s.workerStatusAt = time.Time{}
	s.workerStatusCache = nil
	s.mu.Unlock()
	s.addLog("config saved")
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *AppState) handleStartWorker(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.workerCmd != nil || s.workerStarting {
		s.mu.Unlock()
		writeJSON(w, map[string]string{"status": "already-running"})
		return
	}
	cfg := s.config
	if !cfg.RoleExplicit || cfg.Role != "worker" {
		cfg.Role = "worker"
		cfg.RoleExplicit = true
		s.config = cfg
		_ = saveConfig(cfg)
	}
	s.mu.Unlock()
	if err := s.startWorkerProcess(cfg); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *AppState) startWorkerProcess(cfg Config) error {
	if !cfg.RoleExplicit || cfg.Role != "worker" {
		return fmt.Errorf("worker role is not selected")
	}
	s.mu.Lock()
	if s.workerCmd != nil || s.workerStarting {
		s.mu.Unlock()
		s.addLog("worker start skipped: already running/starting")
		return nil
	}
	s.workerStarting = true
	s.mu.Unlock()
	started := false
	defer func() {
		s.mu.Lock()
		if !started {
			s.workerStarting = false
		}
		s.mu.Unlock()
	}()
	_ = os.MkdirAll(appDir(), 0755)
	bin := llamaBin(cfg.LlamaDir, "rpc-server")
	s.cleanupStaleLlamaProcess("worker", cfg.RPCPort)
	args := []string{"-H", "0.0.0.0", "-p", strconv.Itoa(cfg.RPCPort)}
	args = append(args, workerDeviceArgs(cfg)...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = appDir()
	lw := logWriter{s, "worker"}
	logFile, logPath := processLogWriter("worker")
	if logFile != nil {
		cmd.Stdout = io.MultiWriter(lw, logFile)
		cmd.Stderr = io.MultiWriter(lw, logFile)
	} else {
		cmd.Stdout = lw
		cmd.Stderr = lw
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return err
	}
	if logPath != "" {
		openLogTerminal("ClusterKit worker logs", logPath)
	}
	s.mu.Lock()
	s.workerCmd = cmd
	s.workerStarting = false
	s.workerManualStopped = false
	s.workerStarted = time.Now()
	s.mu.Unlock()
	started = true
	s.addLog("worker started: %s %s", bin, strings.Join(args, " "))
	go s.waitCmd("worker", cmd, logFile)
	return nil
}

func workerDeviceArgs(cfg Config) []string {
	mode := strings.ToLower(strings.TrimSpace(cfg.ComputeMode))
	if runtime.GOOS == "darwin" {
		// macOS workers are always GPU-only for ClusterKit. Exposing CPU/BLAS
		// creates an extra RPC device with no memory report and breaks --fit.
		return []string{"--device", "MTL0"}
	}
	if mode == "" || mode == "auto" {
		return nil
	}
	if runtime.GOOS == "windows" {
		switch mode {
		case "cpu":
			return []string{"--device", "CPU"}
		case "gpu", "cuda":
			return []string{"--device", "CUDA0"}
		}
	}
	return nil
}

func (s *AppState) handleStopWorker(w http.ResponseWriter, r *http.Request) {
	s.stopWorkerManual()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *AppState) handleStartCoordinator(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.serverCmd != nil || s.serverStarting {
		s.mu.Unlock()
		writeJSON(w, map[string]string{"status": "already-running"})
		return
	}
	cfg := s.config
	if !cfg.RoleExplicit || cfg.Role != "coordinator" {
		cfg.Role = "coordinator"
		cfg.RoleExplicit = true
		s.config = cfg
		_ = saveConfig(cfg)
	}
	s.mu.Unlock()
	cfg.Workers = s.classifyWorkers(append([]Worker(nil), cfg.Workers...), 650*time.Millisecond)
	s.mu.Lock()
	s.config = cfg
	s.mu.Unlock()
	_ = saveConfig(cfg)
	if strings.TrimSpace(cfg.ModelPath) == "" {
		http.Error(w, "model path is required", 400)
		return
	}
	if isAuxModelFile(cfg.ModelPath) {
		http.Error(w, "selected file is an auxiliary mmproj/clip file, choose the main model .gguf", 400)
		return
	}
	if err := s.startCoordinatorProcess(cfg, true); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *AppState) startCoordinatorProcess(cfg Config, resetFallback bool) error {
	s.mu.Lock()
	if s.serverCmd != nil || s.serverStarting {
		s.mu.Unlock()
		s.addLog("coordinator start skipped: already running/starting")
		return nil
	}
	if resetFallback && !s.serverLastCrash.IsZero() && time.Since(s.serverLastCrash) < 5*time.Second {
		s.mu.Unlock()
		return fmt.Errorf("coordinator crash cooldown active; wait a few seconds before restart")
	}
	s.serverStarting = true
	s.mu.Unlock()
	started := false
	defer func() {
		s.mu.Lock()
		if !started {
			s.serverStarting = false
		}
		s.mu.Unlock()
	}()
	_ = os.MkdirAll(appDir(), 0755)
	bin := llamaBin(cfg.LlamaDir, "llama-server")
	if len(cfg.Workers) > 0 {
		cfg.Workers = s.waitForRPCWorkers(cfg, 3*time.Second)
	}
	reachableWorkers := policyOnlineWorkers(cfg.Workers, cfg)
	if manualLayerPlan(cfg) {
		cfg = applyManualWorkerLayersToConfig(cfg)
		reachableWorkers = policyOnlineWorkers(cfg.Workers, cfg)
		cfg.GPULayers = manualLayerTotal(cfg, reachableWorkers)
		cfg.SplitMode = "layer"
		s.addLog("manual layer plan active: %s (total GPU/RPC layers %d)", layerPlanSummary(cfg, reachableWorkers), cfg.GPULayers)
		s.mu.Lock()
		s.config.GPULayers = cfg.GPULayers
		s.config.SplitMode = cfg.SplitMode
		s.config.CoordinatorLocal = cfg.CoordinatorLocal
		s.config.CoordinatorLayers = cfg.CoordinatorLayers
		s.mu.Unlock()
		_ = saveConfig(cfg)
	}
	s.cleanupStaleLlamaProcess("coordinator", cfg.APIPort)
	if port, changed := firstFreePort(cfg.APIPort); changed {
		s.addLog("coordinator API port %d is busy; using %d", cfg.APIPort, port)
		cfg.APIPort = port
		s.mu.Lock()
		s.config.APIPort = port
		s.mu.Unlock()
		_ = saveConfig(cfg)
	}
	if strings.EqualFold(cfg.ComputeMode, "cpu") {
		cfg.GPULayers = 0
	}
	if memoryMode(cfg) == "safest" {
		if cfg.Batch > 64 {
			cfg.Batch = 64
		}
		if cfg.UBatch > 32 {
			cfg.UBatch = 32
		}
		if cfg.Parallel > 1 {
			cfg.Parallel = 1
		}
		s.addLog("memory safest mode: clamped batch=%d ubatch=%d parallel=%d", cfg.Batch, cfg.UBatch, cfg.Parallel)
	}
	args := []string{
		"-m", cfg.ModelPath,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(cfg.APIPort),
		"-c", strconv.Itoa(cfg.Context),
		"-t", strconv.Itoa(cfg.Threads),
		"--parallel", strconv.Itoa(cfg.Parallel),
		"--cache-ram", strconv.Itoa(cfg.CacheRAM),
		"-b", strconv.Itoa(cfg.Batch),
		"-ub", strconv.Itoa(cfg.UBatch),
	}
	// Flash Attention, weight repacking, and auto-fit are fast/convenient when
	// they work, but the Windows CUDA/RPC path is much more sensitive to backend
	// and kernel mismatches. On some workers they crash rpc-server with "CUDA
	// error: an illegal memory access" / malformed RPC response even when VRAM is
	// half empty. Default to the safer path.
	args = append(args, "-ngl", strconv.Itoa(cfg.GPULayers), "-sm", splitMode(cfg), "-fa", "off", "--no-repack", "--fit", "off")
	switch memoryMode(cfg) {
	case "normal":
		args = append(args, "--mmap")
	case "mmap":
		args = append(args, "--mmap")
		s.addLog("memory mode: mmap")
	case "safest":
		args = append(args, "--mmap")
		s.addLog("memory mode: safest mmap + small batch")
	}
	if !cfg.CoordinatorLocal && len(reachableWorkers) == 0 {
		// A coordinator with no reachable RPC workers must still be able to run
		// locally. Older configs (and JSON bool zero-values) can leave this false,
		// which made Start look like a no-op in the TUI when workers were absent.
		cfg.CoordinatorLocal = true
		s.mu.Lock()
		s.config.CoordinatorLocal = true
		s.mu.Unlock()
		_ = saveConfig(cfg)
		s.addLog("no reachable RPC workers; enabling local coordinator compute")
	}
	effectiveSplit := coordinatorTensorSplit(cfg, reachableWorkers)
	if effectiveSplit != "" {
		args = append(args, "--tensor-split", effectiveSplit)
		if !cfg.CoordinatorLocal {
			s.addLog("coordinator local compute disabled; tensor split forced to %s", effectiveSplit)
		}
	}
	startedRPC := ""
	if rpc := rpcList(reachableWorkers); rpc != "" {
		args = append(args, "--rpc", rpc)
		if !cfg.CoordinatorLocal {
			args = append(args, "--device", rpcDeviceList(len(reachableWorkers)))
			s.addLog("coordinator local compute disabled; using RPC devices only")
		}
		startedRPC = rpc
		s.addLog("coordinator using %d online RPC worker(s): %s", len(reachableWorkers), rpc)
		if cfg.GPULayers == 0 {
			s.addLog("RPC workers connected but idle: GPU/RPC layers is 0; set GPU/RPC layers > 0 to offload work")
		}
	} else if len(cfg.Workers) > 0 {
		s.addLog("coordinator starting without RPC workers: none reachable right now")
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = appDir()
	lw := logWriter{s, "server"}
	logFile, logPath := processLogWriter("coordinator")
	if logFile != nil {
		cmd.Stdout = io.MultiWriter(lw, logFile)
		cmd.Stderr = io.MultiWriter(lw, logFile)
	} else {
		cmd.Stdout = lw
		cmd.Stderr = lw
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return err
	}
	if logPath != "" {
		openLogTerminal("ClusterKit coordinator logs", logPath)
	}
	s.mu.Lock()
	s.serverCmd = cmd
	s.serverStarting = false
	s.serverLoad = "starting"
	s.serverContext = cfg.Context
	s.serverRPC = startedRPC
	s.loadStarted = time.Now()
	s.loadReady = time.Time{}
	if resetFallback {
		s.serverFallbackTried = false
		s.serverCrashCount = 0
		s.serverLastCrash = time.Time{}
	}
	s.mu.Unlock()
	started = true
	s.addLog("coordinator started: %s %s", bin, strings.Join(args, " "))
	go s.waitCmd("server", cmd, logFile)
	return nil
}

func (s *AppState) handleStopCoordinator(w http.ResponseWriter, r *http.Request) {
	s.stop("server")
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *AppState) handleCheckWorkers(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cfg := s.config
	if isWorkerRole(cfg) {
		workers := append([]Worker(nil), cfg.Workers...)
		s.mu.Unlock()
		writeJSON(w, workers)
		return
	}
	workers := append([]Worker(nil), s.config.Workers...)
	s.mu.Unlock()
	workers = s.classifyWorkers(workers, 650*time.Millisecond)
	s.mu.Lock()
	cfg = s.config
	cfg.Workers = workers
	s.config = cfg
	s.mu.Unlock()
	_ = saveConfig(cfg)
	writeJSON(w, workers)
}

func (s *AppState) workerStatuses() []Worker {
	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()
	return s.workerStatusesFor(cfg)
}

func (s *AppState) workerStatusesFor(cfg Config) []Worker {
	workers := append([]Worker(nil), cfg.Workers...)
	if !isCoordinatorRole(cfg) {
		return workers
	}
	s.mu.Lock()
	if !s.workerStatusAt.IsZero() && time.Since(s.workerStatusAt) < 5*time.Second && len(s.workerStatusCache) == len(workers) {
		cached := append([]Worker(nil), s.workerStatusCache...)
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()
	classified := s.classifyWorkers(workers, 160*time.Millisecond)
	s.mu.Lock()
	s.workerStatusCache = append([]Worker(nil), classified...)
	s.workerStatusAt = time.Now()
	s.mu.Unlock()
	return classified
}

func (s *AppState) classifyWorkers(workers []Worker, timeout time.Duration) []Worker {
	peers := map[string]rememberedPeer{}
	now := time.Now()
	s.mu.Lock()
	for host, item := range s.discovered {
		peers[host] = item
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := range workers {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := workers[i].Port
			if p == 0 {
				p = 50052
			}
			sem <- struct{}{}
			rpcOK := checkTCP(workers[i].Host, p, timeout)
			<-sem
			workers[i].Port = p
			if item, ok := peers[workers[i].Host]; ok {
				age := now.Sub(item.Seen)
				workers[i].SeenMs = age.Milliseconds()
				mergePeerIntoWorker(&workers[i], item.Peer)
				if rpcOK {
					workers[i].OK = true
					workers[i].Status = "connected"
				} else if age < 8*time.Second {
					workers[i].OK = true
					workers[i].Status = "busy/agent"
				} else {
					workers[i].OK = false
					workers[i].Status = "offline"
				}
				return
			}
			workers[i].OK = rpcOK
			if rpcOK {
				workers[i].Status = "connected"
			} else {
				workers[i].Status = "offline"
			}
		}()
	}
	wg.Wait()
	return workers
}

func (s *AppState) waitForRPCWorkers(cfg Config, timeout time.Duration) []Worker {
	deadline := time.Now().Add(timeout)
	workers := append([]Worker(nil), cfg.Workers...)
	best := s.classifyWorkers(workers, 450*time.Millisecond)
	bestOnline := len(policyOnlineWorkers(best, cfg))
	for time.Now().Before(deadline) {
		agentOnly := false
		for _, w := range best {
			if w.Status == "busy/agent" {
				agentOnly = true
				break
			}
		}
		if !agentOnly || bestOnline > 0 {
			return best
		}
		time.Sleep(300 * time.Millisecond)
		candidate := s.classifyWorkers(workers, 450*time.Millisecond)
		candidateOnline := len(policyOnlineWorkers(candidate, cfg))
		if candidateOnline >= bestOnline {
			best = candidate
			bestOnline = candidateOnline
		}
	}
	if bestOnline == 0 {
		s.addLog("self-heal: no RPC workers became reachable before coordinator start")
	}
	return best
}

func mergePeerIntoWorker(w *Worker, peer DiscoveryPeer) {
	if peer.Name != "" {
		w.Name = peer.Name
	}
	if peer.Host != "" {
		w.Host = peer.Host
	}
	if peer.Port != 0 {
		w.Port = peer.Port
	}
	if peer.AppPort != 0 {
		w.AppPort = peer.AppPort
	}
	w.OS = peer.OS
	w.Arch = peer.Arch
	w.RAMBytes = peer.RAM
	w.VRAMBytes = peer.VRAMBytes
	w.Backend = peer.Backend
	w.Threads = peer.Threads
	w.CrashCount = peer.CrashCount
	w.Stability = peer.Stability
	w.RSSBytes = peer.RSSBytes
	w.LoadPct = peer.LoadPct
}

func (s *AppState) handleOpen(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	port := s.config.APIPort
	s.mu.Unlock()
	openBrowser("http://127.0.0.1:" + strconv.Itoa(port))
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *AppState) handleInstall(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("repair") == "1"
	go s.installDeps(force)
	writeJSON(w, map[string]bool{"started": true, "force": force})
}

func (s *AppState) installDeps(force bool) {
	s.installDepsWithOutput(force, nil)
}

func (s *AppState) installDepsConsole(force bool) {
	s.installDepsWithOutput(force, os.Stdout)
}

func (s *AppState) installDepsWithOutput(force bool, console io.Writer) {
	consolef := func(format string, args ...any) {
		if console != nil {
			fmt.Fprintf(console, format+"\n", args...)
		}
	}
	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()
	consolef("ClusterKit install mode: %s", normalizedInstallMode(cfg))
	consolef("llama.cpp dir: %s", cfg.LlamaDir)
	_ = os.MkdirAll(filepath.Dir(cfg.LlamaDir), 0755)
	_ = os.MkdirAll(modelsDir(cfg), 0755)
	st := installStatus(cfg)
	if st.Ready && !force {
		s.addLog("llama.cpp already installed; skipping download")
		consolef("llama.cpp already installed for %s; skipping download", st.Mode)
		s.autostartWorker()
		return
	}
	if !st.Ready && runtime.GOOS == "windows" {
		consolef("installed package not ready for requested mode %s: %s", st.Mode, st.Reason)
	}
	if runtime.GOOS == "windows" {
		consolef(terminalText("Using Windows prebuilt package…"))
		if err := s.installWindowsPrebuilt(cfg, force); err != nil {
			s.addLog("install failed: %v", err)
			consolef("install failed: %v", err)
			return
		}
		s.addLog("install complete")
		consolef("install complete")
		s.autostartWorker()
		return
	}
	var commands [][]string
	switch runtime.GOOS {
	case "darwin":
		commands = [][]string{
			{"bash", "-lc", "command -v brew >/dev/null || /bin/bash -c \"$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\""},
			{"bash", "-lc", "brew install git cmake make || true"},
			{"bash", "-lc", fmt.Sprintf("[ -d %q ] || git clone https://github.com/ggml-org/llama.cpp.git %q", cfg.LlamaDir, cfg.LlamaDir)},
			{"bash", "-lc", fmt.Sprintf("cmake -S %q -B %q -DCMAKE_BUILD_TYPE=Release -DGGML_METAL=ON -DGGML_RPC=ON", cfg.LlamaDir, filepath.Join(cfg.LlamaDir, "build"))},
			{"bash", "-lc", fmt.Sprintf("cmake --build %q --config Release -j $(sysctl -n hw.ncpu)", filepath.Join(cfg.LlamaDir, "build"))},
		}
	case "windows":
		refreshPath := `$env:Path = [Environment]::GetEnvironmentVariable('Path','Machine') + ';' + [Environment]::GetEnvironmentVariable('Path','User') + ';' + $env:LOCALAPPDATA + '\Microsoft\WinGet\Links' + ';C:\Program Files\Git\cmd;C:\Program Files\CMake\bin;C:\Program Files\Ninja;C:\ninja;C:\Program Files\LLVM\bin'; `
		toolVars := `$git = (Get-Command git -ErrorAction SilentlyContinue).Source; if (!$git -and (Test-Path 'C:\Program Files\Git\cmd\git.exe')) { $git = 'C:\Program Files\Git\cmd\git.exe' }; if (!$git) { throw 'git.exe not found after install' }; $cmake = (Get-Command cmake -ErrorAction SilentlyContinue).Source; if (!$cmake -and (Test-Path 'C:\Program Files\CMake\bin\cmake.exe')) { $cmake = 'C:\Program Files\CMake\bin\cmake.exe' }; if (!$cmake) { throw 'cmake.exe not found after install' }; $clang = (Get-Command clang -ErrorAction SilentlyContinue).Source; if (!$clang -and (Test-Path 'C:\Program Files\LLVM\bin\clang.exe')) { $clang = 'C:\Program Files\LLVM\bin\clang.exe' }; if (!$clang) { throw 'clang.exe not found after install' }; $clangxx = (Get-Command clang++ -ErrorAction SilentlyContinue).Source; if (!$clangxx -and (Test-Path 'C:\Program Files\LLVM\bin\clang++.exe')) { $clangxx = 'C:\Program Files\LLVM\bin\clang++.exe' }; if (!$clangxx) { throw 'clang++.exe not found after install' }; `
		installTools := `winget install --id Git.Git -e --source winget --accept-source-agreements --accept-package-agreements; winget install --id Kitware.CMake -e --source winget --accept-source-agreements --accept-package-agreements; winget install --id Ninja-build.Ninja -e --source winget --accept-source-agreements --accept-package-agreements; winget install --id LLVM.LLVM -e --source winget --accept-source-agreements --accept-package-agreements; `
		cloneCmd := fmt.Sprintf("%s%s if (!(Test-Path %q)) { & $git clone https://github.com/ggml-org/llama.cpp.git %q }", refreshPath, toolVars, cfg.LlamaDir, cfg.LlamaDir)
		buildDir := filepath.Join(cfg.LlamaDir, "build")
		cmakeCfgCmd := fmt.Sprintf("%s%s if (Test-Path %q) { Remove-Item -Recurse -Force %q }; & $cmake -S %q -B %q -G Ninja -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_COMPILER=$clang -DCMAKE_CXX_COMPILER=$clangxx -DGGML_RPC=ON -DGGML_VULKAN=ON", refreshPath, toolVars, buildDir, buildDir, cfg.LlamaDir, buildDir)
		cmakeBuildCmd := fmt.Sprintf("%s%s & $cmake --build %q --config Release", refreshPath, toolVars, buildDir)
		commands = [][]string{
			{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", installTools + refreshPath + "git --version; cmake --version; ninja --version; clang --version"},
			{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", cloneCmd},
			{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", cmakeCfgCmd},
			{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", cmakeBuildCmd},
		}
	default:
		commands = [][]string{{"bash", "-lc", "echo unsupported OS for auto install; exit 1"}}
	}
	for _, c := range commands {
		s.addLog("install: %s", strings.Join(c, " "))
		consolef("\n$ %s", strings.Join(c, " "))
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = appDir()
		lw := logWriter{s, "install"}
		if console != nil {
			cmd.Stdout = io.MultiWriter(console, lw)
			cmd.Stderr = io.MultiWriter(console, lw)
		} else {
			cmd.Stdout = lw
			cmd.Stderr = lw
		}
		if err := cmd.Run(); err != nil {
			s.addLog("install failed: %v", err)
			consolef("install failed: %v", err)
			return
		}
	}
	s.addLog("install complete")
	consolef("install complete")
	s.autostartWorker()
}

const discoveryPort = 47777
const discoveryMagic = "clusterkit-discover-v1"

func (s *AppState) workerLoadStats() (uint64, float64) {
	s.mu.Lock()
	cmd := s.workerCmd
	cfg := s.config
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return 0, 0
	}
	rss := processRSSBytes(cmd.Process.Pid)
	if rss == 0 {
		return 0, 0
	}
	usable := workerUsableGB(Worker{OS: runtime.GOOS, Arch: runtime.GOARCH, RAMBytes: totalRAMBytes(), VRAMBytes: 0, Backend: hardwareBackendOnly()})
	if usable <= 0 {
		return rss, 0
	}
	pct := (float64(rss) / (usable * 1024 * 1024 * 1024)) * 100
	if pct > 100 {
		pct = 100
	}
	if pct < 0 {
		pct = 0
	}
	_ = cfg
	return rss, pct
}

func hardwareBackendOnly() string {
	_, b := totalVRAMBytes()
	return b
}

func processRSSBytes(pid int) uint64 {
	if pid <= 0 {
		return 0
	}
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
		if err == nil {
			if kb, e := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); e == nil {
				return kb * 1024
			}
		}
	case "windows":
		cmd := fmt.Sprintf("(Get-Process -Id %d).WorkingSet64", pid)
		out, err := exec.Command("powershell", "-NoProfile", "-Command", cmd).Output()
		if err == nil {
			if v, e := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); e == nil {
				return v
			}
		}
	default:
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
		if err == nil {
			parts := strings.Fields(string(b))
			if len(parts) >= 2 {
				pages, e := strconv.ParseUint(parts[1], 10, 64)
				if e == nil {
					return pages * uint64(os.Getpagesize())
				}
			}
		}
	}
	return 0
}

func (s *AppState) discoveryResponder() {
	addr := net.UDPAddr{IP: net.IPv4zero, Port: discoveryPort}
	conn, err := net.ListenUDP("udp4", &addr)
	if err != nil {
		s.addLog("discovery disabled: %v", err)
		return
	}
	defer conn.Close()
	buf := make([]byte, 1024)
	for {
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		payload := buf[:n]
		var announced DiscoveryPeer
		if json.Unmarshal(payload, &announced) == nil && announced.Host != "" {
			if announced.Host == localIP() {
				continue
			}
			s.rememberPeer(announced)
			continue
		}
		if strings.TrimSpace(string(payload)) != discoveryMagic {
			continue
		}
		s.mu.Lock()
		cfg := s.config
		s.mu.Unlock()
		if !cfg.RoleExplicit || cfg.Role != "worker" {
			continue
		}
		hw := hardwareInfo()
		crashes, stability := s.workerHealthStats()
		rss, loadPct := s.workerLoadStats()
		backend, vram := effectiveBackend(cfg, hw)
		peer := DiscoveryPeer{Name: hw.Hostname, Host: localIP(), Port: cfg.RPCPort, AppPort: s.appPort, Role: cfg.Role, OS: hw.OS, Arch: hw.Arch, RAM: hw.RAMBytes, VRAMBytes: vram, Backend: backend, Threads: hw.CPUCount, CrashCount: crashes, Stability: stability, RSSBytes: rss, LoadPct: loadPct}
		b, _ := json.Marshal(peer)
		_, _ = conn.WriteToUDP(b, remote)
	}
}

func (s *AppState) discoveryAnnouncer() {
	t := time.NewTicker(1000 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		cfg := s.config
		running := s.workerCmd != nil && s.workerCmd.Process != nil
		s.mu.Unlock()
		if !cfg.RoleExplicit || cfg.Role != "worker" || !running {
			continue
		}
		hw := hardwareInfo()
		crashes, stability := s.workerHealthStats()
		rss, loadPct := s.workerLoadStats()
		backend, vram := effectiveBackend(cfg, hw)
		peer := DiscoveryPeer{Name: hw.Hostname, Host: localIP(), Port: cfg.RPCPort, AppPort: s.appPort, Role: "worker", OS: hw.OS, Arch: hw.Arch, RAM: hw.RAMBytes, VRAMBytes: vram, Backend: backend, Threads: hw.CPUCount, CrashCount: crashes, Stability: stability, RSSBytes: rss, LoadPct: loadPct}
		broadcastPeer(peer)
	}
}

func broadcastPeer(peer DiscoveryPeer) {
	b, _ := json.Marshal(peer)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return
	}
	defer conn.Close()
	seen := map[string]bool{}
	for _, ip := range append([]string{"255.255.255.255"}, subnetBroadcasts()...) {
		if seen[ip] {
			continue
		}
		seen[ip] = true
		_ = conn.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
		_, _ = conn.WriteToUDP(b, &net.UDPAddr{IP: net.ParseIP(ip), Port: discoveryPort})
	}
}

func (s *AppState) rememberPeer(peer DiscoveryPeer) {
	if peer.Role != "worker" || peer.Host == "" {
		return
	}
	if peer.Port == 0 {
		peer.Port = 50052
	}
	s.mu.Lock()
	if s.discovered == nil {
		s.discovered = map[string]rememberedPeer{}
	}
	s.discovered[peer.Host] = rememberedPeer{Peer: peer, Seen: time.Now()}
	s.mu.Unlock()
}

func (s *AppState) rememberedPeers(maxAge time.Duration) []DiscoveryPeer {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []DiscoveryPeer{}
	for host, item := range s.discovered {
		if now.Sub(item.Seen) > maxAge {
			delete(s.discovered, host)
			continue
		}
		out = append(out, item.Peer)
	}
	return out
}

func (s *AppState) handleDiscover(w http.ResponseWriter, r *http.Request) {
	seen := map[string]DiscoveryPeer{}
	for _, peer := range s.rememberedPeers(30 * time.Second) {
		seen[peer.Host] = peer
	}
	for _, peer := range discoverPeers(1400 * time.Millisecond) {
		seen[peer.Host] = peer
		s.rememberPeer(peer)
	}
	peers := make([]DiscoveryPeer, 0, len(seen))
	for _, peer := range seen {
		peers = append(peers, peer)
	}
	writeJSON(w, peers)
}

func discoverPeers(timeout time.Duration) []DiscoveryPeer {
	seen := map[string]DiscoveryPeer{}
	selfIPs := localIPs()
	isSelf := map[string]bool{}
	for _, ip := range selfIPs {
		isSelf[ip] = true
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err == nil {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(timeout))
		msg := []byte(discoveryMagic)
		broadcasts := []net.IP{net.IPv4bcast}
		for _, selfIP := range selfIPs {
			ip := net.ParseIP(selfIP).To4()
			if ip == nil {
				continue
			}
			broadcasts = append(broadcasts, net.IPv4(ip[0], ip[1], ip[2], 255))
			// Broadcast is flaky on some Windows/router combinations. Also send
			// direct UDP probes across the common /24 LAN.
			for i := 1; i <= 254; i++ {
				candidate := net.IPv4(ip[0], ip[1], ip[2], byte(i))
				if !isSelf[candidate.String()] {
					_, _ = conn.WriteToUDP(msg, &net.UDPAddr{IP: candidate, Port: discoveryPort})
				}
			}
		}
		for _, ip := range broadcasts {
			_, _ = conn.WriteToUDP(msg, &net.UDPAddr{IP: ip, Port: discoveryPort})
		}
		buf := make([]byte, 4096)
		for {
			n, remote, err := conn.ReadFromUDP(buf)
			if err != nil {
				break
			}
			var peer DiscoveryPeer
			if json.Unmarshal(buf[:n], &peer) == nil {
				if peer.Host == "" {
					peer.Host = remote.IP.String()
				}
				if isSelf[peer.Host] {
					continue
				}
				if peer.Role == "worker" {
					seen[peer.Host] = peer
				}
			}
		}
	}

	// Fallback: if UDP discovery is blocked by firewall, find running RPC
	// workers by scanning the local /24 for port 50052.
	for _, selfIP := range selfIPs {
		for _, peer := range scanRPCWorkers(selfIP, 50052, 550*time.Millisecond) {
			if !isSelf[peer.Host] {
				if _, ok := seen[peer.Host]; !ok {
					seen[peer.Host] = peer
				}
			}
		}
	}

	out := make([]DiscoveryPeer, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	return out
}

func scanRPCWorkers(self string, port int, timeout time.Duration) []DiscoveryPeer {
	ip := net.ParseIP(self).To4()
	if ip == nil {
		return nil
	}
	out := []DiscoveryPeer{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	for i := 1; i <= 254; i++ {
		candidate := net.IPv4(ip[0], ip[1], ip[2], byte(i)).String()
		if candidate == self {
			continue
		}
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ok := checkTCP(host, port, timeout)
			if ok {
				name := host
				if names, err := net.LookupAddr(host); err == nil && len(names) > 0 {
					name = strings.TrimSuffix(names[0], ".")
				}
				mu.Lock()
				out = append(out, DiscoveryPeer{Name: name, Host: host, Port: port, Role: "worker"})
				mu.Unlock()
			}
		}(candidate)
	}
	wg.Wait()
	return out
}

func (s *AppState) handleOptimize(w http.ResponseWriter, r *http.Request) {
	hw := hardwareInfo()
	rec := recommendSettings(hw)
	s.mu.Lock()
	cfg := s.config
	cfg.Context = rec.Context
	cfg.GPULayers = rec.GPULayers
	cfg.Threads = rec.Threads
	s.config = cfg
	s.mu.Unlock()
	_ = saveConfig(cfg)
	s.addLog("optimized: context=%d gpuLayers=%d threads=%d", rec.Context, rec.GPULayers, rec.Threads)
	writeJSON(w, rec)
}

func workerUsableGB(w Worker) float64 {
	ramGB := float64(w.RAMBytes) / 1024 / 1024 / 1024
	vramGB := float64(w.VRAMBytes) / 1024 / 1024 / 1024
	if strings.EqualFold(w.Backend, "CPU") || strings.EqualFold(w.Backend, "CPU/unknown") || strings.Contains(strings.ToLower(w.Backend), "cpu") {
		if ramGB > 0 {
			v := ramGB * 0.55
			if ramGB-4 < v {
				v = ramGB - 4
			}
			if v < 2 {
				v = 2
			}
			return v
		}
	}
	if vramGB > 0 {
		return vramGB * 0.90
	}
	if w.OS == "darwin" && strings.Contains(w.Arch, "arm64") {
		reserve := 4.0
		factor := 0.45
		if ramGB <= 10 {
			reserve = 5.5
			factor = 0.28
		}
		v := ramGB * factor
		if ramGB-reserve < v {
			v = ramGB - reserve
		}
		if v < 0 {
			return 0
		}
		return v
	}
	if ramGB > 0 {
		v := ramGB * 0.45
		if v < 2 {
			v = 2
		}
		return v
	}
	return 4
}

type Recommendation struct {
	Context   int    `json:"context"`
	GPULayers int    `json:"gpuLayers"`
	Threads   int    `json:"threads"`
	Reason    string `json:"reason"`
}

func recommendSettings(hw HardwareInfo) Recommendation {
	ramGB := hw.RAMBytes / 1024 / 1024 / 1024
	ctx := 4096
	if ramGB >= 32 {
		ctx = 8192
	}
	if ramGB >= 64 {
		ctx = 16384
	}
	gpu := 0
	if hw.OS == "darwin" && strings.Contains(hw.Arch, "arm64") {
		gpu = 20
		if ramGB >= 32 {
			gpu = 60
		}
		if ramGB >= 64 {
			gpu = 99
		}
	}
	threads := max(1, hw.CPUCount-1)
	if hw.CPUCount >= 8 {
		threads = hw.CPUCount - 2
	}
	return Recommendation{Context: ctx, GPULayers: gpu, Threads: threads, Reason: fmt.Sprintf("%s/%s, %d CPU threads, ~%d GB RAM", hw.OS, hw.Arch, hw.CPUCount, ramGB)}
}

func hardwareInfo() HardwareInfo {
	hardwareInfoCache.Lock()
	if !hardwareInfoCache.at.IsZero() && time.Since(hardwareInfoCache.at) < 30*time.Second {
		v := hardwareInfoCache.value
		hardwareInfoCache.Unlock()
		return v
	}
	hardwareInfoCache.Unlock()
	host, _ := os.Hostname()
	vram, backend := totalVRAMBytes()
	hw := HardwareInfo{Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUCount: runtime.NumCPU(), RAMBytes: totalRAMBytes(), VRAMBytes: vram, Backend: backend}
	hardwareInfoCache.Lock()
	hardwareInfoCache.value = hw
	hardwareInfoCache.at = time.Now()
	hardwareInfoCache.Unlock()
	return hw
}

func totalRAMBytes() uint64 {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			if v, e := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); e == nil {
				return v
			}
		}
	case "windows":
		out, err := exec.Command("powershell", "-NoProfile", "-Command", "(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory").Output()
		if err == nil {
			if v, e := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); e == nil {
				return v
			}
		}
	default:
		b, err := os.ReadFile("/proc/meminfo")
		if err == nil {
			re := regexp.MustCompile(`MemTotal:\\s+(\\d+)\\s+kB`)
			m := re.FindStringSubmatch(string(b))
			if len(m) == 2 {
				if kb, e := strconv.ParseUint(m[1], 10, 64); e == nil {
					return kb * 1024
				}
			}
		}
	}
	return 0
}

func totalVRAMBytes() (uint64, string) {
	if out, err := exec.Command("nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits").Output(); err == nil {
		var total uint64
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if mb, e := strconv.ParseUint(strings.Fields(line)[0], 10, 64); e == nil {
				total += mb * 1024 * 1024
			}
		}
		if total > 0 {
			return total, "CUDA"
		}
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return 0, "Metal unified memory"
	}
	return 0, "CPU/unknown"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type InstallStatus struct {
	Ready  bool   `json:"ready"`
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
}

func normalizedInstallMode(cfg Config) string {
	mode := strings.ToLower(strings.TrimSpace(cfg.ComputeMode))
	if runtime.GOOS == "windows" && mode == "cpu" {
		return "cpu"
	}
	if runtime.GOOS == "windows" && (mode == "gpu" || mode == "cuda") {
		return "gpu"
	}
	if runtime.GOOS == "windows" {
		return "gpu"
	}
	return "default"
}

func installStatus(cfg Config) InstallStatus {
	if !fileExists(llamaBin(cfg.LlamaDir, "rpc-server")) || !fileExists(llamaBin(cfg.LlamaDir, "llama-server")) {
		return InstallStatus{Ready: false, Mode: normalizedInstallMode(cfg), Reason: "rpc-server/llama-server missing"}
	}
	mode := normalizedInstallMode(cfg)
	markerPath := filepath.Join(cfg.LlamaDir, ".clusterkit-package")
	marker := ""
	if b, err := os.ReadFile(markerPath); err == nil {
		marker = strings.TrimSpace(string(b))
	}
	if runtime.GOOS == "windows" {
		if marker == "" {
			return InstallStatus{Ready: false, Mode: mode, Reason: "package marker missing; repair required"}
		}
		if marker != mode {
			return InstallStatus{Ready: false, Mode: mode, Reason: "installed package is " + marker + ", need " + mode}
		}
	}
	return InstallStatus{Ready: true, Mode: mode, Reason: "installed"}
}

func effectiveBackend(cfg Config, hw HardwareInfo) (string, uint64) {
	mode := strings.ToLower(strings.TrimSpace(cfg.ComputeMode))
	if runtime.GOOS == "darwin" {
		return "Metal unified memory", hw.VRAMBytes
	}
	if runtime.GOOS == "windows" && mode == "cpu" {
		return "CPU", 0
	}
	return hw.Backend, hw.VRAMBytes
}

type ghRelease struct {
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (s *AppState) installWindowsPrebuilt(cfg Config, force bool) error {
	st := installStatus(cfg)
	if st.Ready && !force {
		s.addLog("windows: %s package already installed; skipping download", st.Mode)
		return nil
	}
	s.addLog("windows: using official llama.cpp prebuilt binaries for %s", normalizedInstallMode(cfg))
	var rel ghRelease
	if err := getJSON("https://api.github.com/repos/ggml-org/llama.cpp/releases/latest", &rel); err != nil {
		return err
	}
	assetURL := ""
	cudartURL := ""
	cudaVersion := ""
	if normalizedInstallMode(cfg) != "cpu" {
		for _, a := range rel.Assets {
			name := strings.ToLower(a.Name)
			if strings.Contains(name, "bin-win-cuda-12.4-x64.zip") && !strings.HasPrefix(name, "cudart-") {
				assetURL = a.URL
				cudaVersion = "12.4"
				break
			}
		}
		if assetURL == "" {
			for _, a := range rel.Assets {
				name := strings.ToLower(a.Name)
				if strings.Contains(name, "bin-win-cuda") && strings.Contains(name, "x64.zip") && !strings.HasPrefix(name, "cudart-") {
					assetURL = a.URL
					if strings.Contains(name, "cuda-13.1") {
						cudaVersion = "13.1"
					} else if strings.Contains(name, "cuda-12.4") {
						cudaVersion = "12.4"
					}
					break
				}
			}
		}
	}
	if cudaVersion != "" {
		for _, a := range rel.Assets {
			name := strings.ToLower(a.Name)
			if strings.HasPrefix(name, "cudart-") && strings.Contains(name, "cuda-"+cudaVersion+"-x64.zip") {
				cudartURL = a.URL
				break
			}
		}
	}
	if assetURL == "" && normalizedInstallMode(cfg) == "cpu" {
		for _, a := range rel.Assets {
			name := strings.ToLower(a.Name)
			if strings.Contains(name, "bin-win-cpu-x64.zip") && !strings.Contains(name, "cudart") {
				assetURL = a.URL
				break
			}
		}
	}
	if assetURL == "" {
		for _, a := range rel.Assets {
			name := strings.ToLower(a.Name)
			if strings.Contains(name, "bin-win-vulkan-x64.zip") {
				assetURL = a.URL
				break
			}
		}
	}
	if assetURL == "" {
		for _, a := range rel.Assets {
			name := strings.ToLower(a.Name)
			if strings.Contains(name, "bin-win-cpu-x64.zip") && !strings.Contains(name, "cudart") {
				assetURL = a.URL
				break
			}
		}
	}
	if assetURL == "" {
		return fmt.Errorf("no suitable llama.cpp Windows release asset found")
	}
	if force {
		s.addLog("windows: force repair requested; replacing llama.cpp files")
		_ = os.RemoveAll(cfg.LlamaDir)
	}
	if err := os.MkdirAll(cfg.LlamaDir, 0755); err != nil {
		return err
	}
	zipPath := filepath.Join(appDir(), "llama-win.zip")
	if err := downloadFile(assetURL, zipPath, func(got, total int64) {
		if total > 0 {
			s.addLog("download llama.cpp %.1f / %.1f MB", float64(got)/1024/1024, float64(total)/1024/1024)
		} else {
			s.addLog("download llama.cpp %.1f MB", float64(got)/1024/1024)
		}
	}); err != nil {
		return err
	}
	defer os.Remove(zipPath)
	if err := unzip(zipPath, cfg.LlamaDir); err != nil {
		return err
	}
	if cudartURL != "" {
		s.addLog("windows: installing CUDA runtime %s", cudaVersion)
		cudartZip := filepath.Join(appDir(), "llama-win-cudart.zip")
		if err := downloadFile(cudartURL, cudartZip, func(got, total int64) {
			if total > 0 {
				s.addLog("download CUDA runtime %.1f / %.1f MB", float64(got)/1024/1024, float64(total)/1024/1024)
			} else {
				s.addLog("download CUDA runtime %.1f MB", float64(got)/1024/1024)
			}
		}); err != nil {
			return err
		}
		defer os.Remove(cudartZip)
		if err := unzip(cudartZip, cfg.LlamaDir); err != nil {
			return err
		}
	}
	if !fileExists(llamaBin(cfg.LlamaDir, "rpc-server")) || !fileExists(llamaBin(cfg.LlamaDir, "llama-server")) {
		return fmt.Errorf("downloaded llama.cpp but rpc-server/llama-server were not found")
	}
	_ = os.WriteFile(filepath.Join(cfg.LlamaDir, ".clusterkit-package"), []byte(normalizedInstallMode(cfg)), 0644)
	return nil
}

func downloadFile(src, dst string, progress func(got, total int64)) error {
	resp, err := http.Get(src)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("download failed: %s", resp.Status)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	buf := make([]byte, 1024*1024)
	var got int64
	total := resp.ContentLength
	last := time.Now().Add(-10 * time.Second)
	for {
		n, er := resp.Body.Read(buf)
		if n > 0 {
			if _, ew := out.Write(buf[:n]); ew != nil {
				return ew
			}
			got += int64(n)
		}
		if progress != nil && time.Since(last) > 2*time.Second {
			progress(got, total)
			last = time.Now()
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			return er
		}
	}
	if progress != nil {
		progress(got, total)
	}
	return nil
}

func unzip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	cleanDst, _ := filepath.Abs(dst)
	for _, f := range r.File {
		outPath := filepath.Join(dst, f.Name)
		absOut, _ := filepath.Abs(outPath)
		if !strings.HasPrefix(absOut, cleanDst+string(os.PathSeparator)) && absOut != cleanDst {
			return fmt.Errorf("unsafe zip path: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(outPath, 0755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

type HFModel struct {
	ID        string `json:"id"`
	Downloads int    `json:"downloads"`
	Likes     int    `json:"likes"`
	UpdatedAt string `json:"lastModified"`
}
type HFSibling struct {
	RFilename string `json:"rfilename"`
	Size      int64  `json:"size"`
}
type HFModelInfo struct {
	ID       string      `json:"id"`
	Siblings []HFSibling `json:"siblings"`
}

func (s *AppState) handleModelSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	out, err := hfSearchModels(q)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	writeJSON(w, out)
}

func hfSearchModels(q string) ([]HFModel, error) {
	q = defaultHFQuery(q)
	u := "https://huggingface.co/api/models?" + url.Values{"search": {q}, "filter": {"gguf"}, "sort": {"downloads"}, "direction": {"-1"}, "limit": {"20"}}.Encode()
	var out []HFModel
	return out, getJSON(u, &out)
}

func (s *AppState) handleModelFiles(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo == "" {
		http.Error(w, "repo required", 400)
		return
	}
	files, err := hfModelFiles(repo)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	writeJSON(w, files)
}

func hfModelFiles(repo string) ([]HFSibling, error) {
	var info HFModelInfo
	if err := getJSON("https://huggingface.co/api/models/"+repo, &info); err != nil {
		return nil, err
	}
	files := []HFSibling{}
	for _, f := range info.Siblings {
		if strings.HasSuffix(strings.ToLower(f.RFilename), ".gguf") {
			if f.Size <= 0 {
				f.Size = hfFileSize(repo, f.RFilename)
			}
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Size < files[j].Size })
	return files, nil
}

func hfFileSize(repo, filename string) int64 {
	client := &http.Client{Timeout: 12 * time.Second}
	req, err := http.NewRequest("HEAD", "https://huggingface.co/"+repo+"/resolve/main/"+escapeHFPath(filename), nil)
	if err != nil {
		return 0
	}
	req.Header.Set("User-Agent", "ClusterKit/0.1")
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0
	}
	if resp.ContentLength < 0 {
		return 0
	}
	return resp.ContentLength
}

func escapeHFPath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func (s *AppState) handleLocalModels(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()
	root := modelsDir(cfg)
	_ = os.MkdirAll(root, 0755)
	type local struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		Size       int64  `json:"size"`
		Selected   bool   `json:"selected"`
		Aux        bool   `json:"aux"`
		MaxContext int    `json:"maxContext,omitempty"`
	}
	items := []local{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".gguf") {
			if st, e := d.Info(); e == nil {
				maxCtx, _ := ggufMaxContext(path)
				items = append(items, local{d.Name(), path, st.Size(), path == cfg.ModelPath, isAuxModelFile(path), maxCtx})
			}
		}
		return nil
	})
	writeJSON(w, items)
}

func ggufMaxContext(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return 0, err
	}
	if string(magic[:]) != "GGUF" {
		return 0, fmt.Errorf("not a GGUF file")
	}
	var version uint32
	var tensorCount, metaCount uint64
	if err := binary.Read(f, binary.LittleEndian, &version); err != nil {
		return 0, err
	}
	if err := binary.Read(f, binary.LittleEndian, &tensorCount); err != nil {
		return 0, err
	}
	if err := binary.Read(f, binary.LittleEndian, &metaCount); err != nil {
		return 0, err
	}
	for i := uint64(0); i < metaCount; i++ {
		key, err := ggufReadString(f)
		if err != nil {
			return 0, err
		}
		var typ uint32
		if err := binary.Read(f, binary.LittleEndian, &typ); err != nil {
			return 0, err
		}
		if strings.HasSuffix(key, ".context_length") {
			v, err := ggufReadIntValue(f, typ)
			if err != nil {
				return 0, err
			}
			if v > 0 && v <= int64(^uint(0)>>1) {
				return int(v), nil
			}
			return 0, nil
		}
		if err := ggufSkipValue(f, typ); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func ggufReadString(r io.Reader) (string, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n > 1<<20 {
		return "", fmt.Errorf("gguf string too large")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return string(b), err
}

func ggufReadIntValue(r io.Reader, typ uint32) (int64, error) {
	switch typ {
	case 0:
		var v uint8
		err := binary.Read(r, binary.LittleEndian, &v)
		return int64(v), err
	case 1:
		var v int8
		err := binary.Read(r, binary.LittleEndian, &v)
		return int64(v), err
	case 2:
		var v uint16
		err := binary.Read(r, binary.LittleEndian, &v)
		return int64(v), err
	case 3:
		var v int16
		err := binary.Read(r, binary.LittleEndian, &v)
		return int64(v), err
	case 4:
		var v uint32
		err := binary.Read(r, binary.LittleEndian, &v)
		return int64(v), err
	case 5:
		var v int32
		err := binary.Read(r, binary.LittleEndian, &v)
		return int64(v), err
	case 10:
		var v uint64
		err := binary.Read(r, binary.LittleEndian, &v)
		if v > uint64(^uint(0)>>1) {
			return 0, fmt.Errorf("gguf int too large")
		}
		return int64(v), err
	case 11:
		var v int64
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	default:
		return 0, ggufSkipValue(r, typ)
	}
}

func ggufSkipValue(r io.Reader, typ uint32) error {
	skip := func(n int64) error { _, err := io.CopyN(io.Discard, r, n); return err }
	switch typ {
	case 0, 1, 7:
		return skip(1)
	case 2, 3:
		return skip(2)
	case 4, 5, 6:
		return skip(4)
	case 10, 11, 12:
		return skip(8)
	case 8:
		_, err := ggufReadString(r)
		return err
	case 9:
		var elemTyp uint32
		var n uint64
		if err := binary.Read(r, binary.LittleEndian, &elemTyp); err != nil {
			return err
		}
		if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
			return err
		}
		for i := uint64(0); i < n; i++ {
			if err := ggufSkipValue(r, elemTyp); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown gguf metadata type %d", typ)
	}
}

func (s *AppState) handleModelDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo string `json:"repo"`
		File string `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.Repo == "" || req.File == "" {
		http.Error(w, "repo and file required", 400)
		return
	}
	s.mu.Lock()
	active := s.download.Active
	s.mu.Unlock()
	if active {
		http.Error(w, "another model download is already running", 409)
		return
	}
	go s.downloadModel(req.Repo, req.File)
	writeJSON(w, map[string]bool{"started": true})
}

func (s *AppState) updateDownload(fn func(*DownloadStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.download)
	if s.download.Total > 0 {
		s.download.Percent = int((s.download.Downloaded * 100) / s.download.Total)
		if s.download.Percent > 100 {
			s.download.Percent = 100
		}
	}
	s.download.UpdatedMs = time.Now().UnixMilli()
}

func (s *AppState) downloadModel(repo, file string) {
	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()
	dir := filepath.Join(modelsDir(cfg), strings.NewReplacer("/", "__").Replace(repo))
	_ = os.MkdirAll(dir, 0755)
	dst := filepath.Join(dir, filepath.Base(file))
	tmp := dst + ".part"
	u := "https://huggingface.co/" + repo + "/resolve/main/" + url.PathEscape(file)
	u = strings.ReplaceAll(u, "%2F", "/")
	started := time.Now()
	s.updateDownload(func(d *DownloadStatus) {
		*d = DownloadStatus{Active: true, Repo: repo, File: file, Path: dst, Status: "connecting", StartedMs: started.UnixMilli(), UpdatedMs: started.UnixMilli()}
	})
	s.addLog("download: %s", u)
	resp, err := http.Get(u)
	if err != nil {
		s.updateDownload(func(d *DownloadStatus) { d.Active = false; d.Status = "failed"; d.Error = err.Error() })
		s.addLog("download failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		s.updateDownload(func(d *DownloadStatus) { d.Active = false; d.Status = "failed"; d.Error = resp.Status })
		s.addLog("download failed: %s", resp.Status)
		return
	}
	s.updateDownload(func(d *DownloadStatus) { d.Status = "downloading"; d.Total = resp.ContentLength })
	out, err := os.Create(tmp)
	if err != nil {
		s.updateDownload(func(d *DownloadStatus) { d.Active = false; d.Status = "failed"; d.Error = err.Error() })
		s.addLog("download failed: %v", err)
		return
	}
	defer out.Close()
	buf := make([]byte, 1024*1024)
	var got int64
	last := time.Now()
	for {
		n, er := resp.Body.Read(buf)
		if n > 0 {
			if _, ew := out.Write(buf[:n]); ew != nil {
				s.updateDownload(func(d *DownloadStatus) { d.Active = false; d.Status = "failed"; d.Error = ew.Error() })
				s.addLog("download failed: %v", ew)
				return
			}
			got += int64(n)
			elapsed := time.Since(started).Seconds()
			speed := int64(0)
			if elapsed > 0 {
				speed = int64(float64(got) / elapsed)
			}
			s.updateDownload(func(d *DownloadStatus) { d.Downloaded = got; d.SpeedBps = speed; d.Status = "downloading" })
		}
		if time.Since(last) > 2*time.Second {
			s.addLog("download %.1f MB: %s", float64(got)/1024/1024, filepath.Base(file))
			last = time.Now()
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			s.updateDownload(func(d *DownloadStatus) { d.Active = false; d.Status = "failed"; d.Error = er.Error() })
			s.addLog("download failed: %v", er)
			return
		}
	}
	_ = out.Close()
	if err := os.Rename(tmp, dst); err != nil {
		s.updateDownload(func(d *DownloadStatus) { d.Active = false; d.Status = "failed"; d.Error = err.Error() })
		s.addLog("download failed: %v", err)
		return
	}
	s.updateDownload(func(d *DownloadStatus) {
		d.Active = false
		d.Status = "complete"
		d.Downloaded = got
		d.Percent = 100
		d.SpeedBps = 0
	})
	s.addLog("download complete: %s", dst)
}

func (s *AppState) handleModelSelect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if isAuxModelFile(req.Path) {
		http.Error(w, "cannot select auxiliary mmproj/clip file as main model", 400)
		return
	}
	s.mu.Lock()
	cfg := s.config
	cfg.ModelPath = req.Path
	s.config = cfg
	s.mu.Unlock()
	_ = saveConfig(cfg)
	s.addLog("selected model: %s", req.Path)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *AppState) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()
	root, _ := filepath.Abs(modelsDir(cfg))
	target, _ := filepath.Abs(req.Path)
	if !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		http.Error(w, "refusing to delete outside models dir", 400)
		return
	}
	if err := os.Remove(target); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if cfg.ModelPath == req.Path {
		cfg.ModelPath = ""
		s.mu.Lock()
		s.config = cfg
		s.mu.Unlock()
		_ = saveConfig(cfg)
	}
	s.addLog("deleted model: %s", req.Path)
	writeJSON(w, map[string]bool{"ok": true})
}

type CacheClearRequest struct {
	All          bool   `json:"all"`
	KeepSelected bool   `json:"keepSelected"`
	Path         string `json:"path"`
}

func (s *AppState) handleModelCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req CacheClearRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		http.Error(w, err.Error(), 400)
		return
	}
	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()
	removed, err := clearModelCache(modelsDir(cfg), cfg.ModelPath, req.KeepSelected && !req.All, req.Path)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if req.All || (cfg.ModelPath != "" && !fileExists(cfg.ModelPath)) {
		cfg.ModelPath = ""
		s.mu.Lock()
		s.config = cfg
		s.mu.Unlock()
		_ = saveConfig(cfg)
	}
	s.addLog("local model cache clear: removed %d file(s)", removed)
	writeJSON(w, map[string]any{"ok": true, "removed": removed})
}

func clearModelCache(root, selected string, keepSelected bool, onlyPath string) (int, error) {
	cleanRoot, _ := filepath.Abs(root)
	selectedAbs, _ := filepath.Abs(selected)
	onlyAbs := ""
	if strings.TrimSpace(onlyPath) != "" {
		onlyAbs, _ = filepath.Abs(onlyPath)
		if !strings.HasPrefix(onlyAbs, cleanRoot+string(os.PathSeparator)) {
			return 0, fmt.Errorf("refusing to delete outside models dir")
		}
	}
	removed := 0
	err := filepath.WalkDir(cleanRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".gguf") {
			return nil
		}
		abs, _ := filepath.Abs(path)
		if onlyAbs != "" && abs != onlyAbs {
			return nil
		}
		if keepSelected && selectedAbs != "" && abs == selectedAbs {
			return nil
		}
		if err := os.Remove(abs); err != nil {
			return err
		}
		removed++
		return nil
	})
	return removed, err
}

func (s *AppState) handleOpenAIRoot(w http.ResponseWriter, r *http.Request) {
	setOpenAIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	writeJSON(w, map[string]any{
		"object":    "clusterkit.openai_compat",
		"status":    "ok",
		"endpoints": []string{"/v1/models", "/v1/chat/completions", "/v1/completions", "/v1/health"},
	})
}

func (s *AppState) handleOpenAIHealth(w http.ResponseWriter, r *http.Request) {
	setOpenAIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	s.mu.Lock()
	cfg := s.config
	appPort := s.appPort
	running := s.serverCmd != nil && s.serverCmd.Process != nil
	load := s.serverLoad
	s.mu.Unlock()
	reachable := coordinatorPortReachable(cfg.APIPort)
	status := "unavailable"
	if running || reachable {
		status = "ok"
	}
	writeJSON(w, map[string]any{
		"status":               status,
		"openaiBaseURL":        fmt.Sprintf("http://127.0.0.1:%d/v1", appPort),
		"coordinatorAPI":       fmt.Sprintf("http://127.0.0.1:%d/v1", cfg.APIPort),
		"coordinatorRunning":   running,
		"coordinatorReachable": reachable,
		"serverLoad":           load,
		"model":                filepath.Base(cfg.ModelPath),
	})
}

func coordinatorPortReachable(port int) bool {
	if port <= 0 {
		return false
	}
	return checkTCP("127.0.0.1", port, 220*time.Millisecond)
}

func (s *AppState) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	setOpenAIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()
	// Keep the public model id short and stable. Some chat surfaces put model ids
	// into inline button callback payloads; long GGUF filenames can make the
	// picker show "1 available" with no usable button.
	writeJSON(w, openAIModelsResponse(cfg))
}

func openAIModelsResponse(cfg Config) map[string]any {
	modelPath := strings.TrimSpace(cfg.ModelPath)
	modelName := "clusterkit-local"
	if modelPath != "" {
		modelName = filepath.Base(modelPath)
	}
	return map[string]any{
		"object": "list",
		"data": []map[string]any{{
			"id":       "clusterkit-local",
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "clusterkit",
			"name":     modelName,
		}},
	}
}

func (s *AppState) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handleOpenAIProxy(w, r, "/v1/chat/completions")
}

func (s *AppState) handleOpenAICompletions(w http.ResponseWriter, r *http.Request) {
	s.handleOpenAIProxy(w, r, "/v1/completions")
}

func (s *AppState) handleOpenAIProxy(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	setOpenAIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	s.mu.Lock()
	port := s.config.APIPort
	running := s.serverCmd != nil && s.serverCmd.Process != nil
	if running {
		s.serverLoad = "processing"
	}
	s.mu.Unlock()
	if !running && !coordinatorPortReachable(port) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "coordinator_unavailable", fmt.Sprintf("ClusterKit coordinator API is not reachable on 127.0.0.1:%d", port))
		return
	}
	if !proxyOpenAIRequest(w, r, port, upstreamPath) {
		s.mu.Lock()
		if s.serverCmd != nil && s.serverCmd.Process != nil {
			s.serverLoad = "unreachable"
		}
		s.mu.Unlock()
	}
}

func proxyOpenAIRequest(w http.ResponseWriter, r *http.Request, port int, upstreamPath string) bool {
	if port == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "coordinator_unavailable", "coordinator API port is not configured")
		return false
	}
	upURL := "http://127.0.0.1:" + strconv.Itoa(port) + upstreamPath
	if r.URL.RawQuery != "" {
		upURL += "?" + r.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upURL, r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	copyProxyHeaders(upReq.Header, r.Header)
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(upReq)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "coordinator_unavailable", "coordinator API unavailable: "+err.Error())
		return false
	}
	defer resp.Body.Close()
	for k, vals := range resp.Header {
		low := strings.ToLower(k)
		if low == "content-length" || low == "connection" || low == "keep-alive" || low == "transfer-encoding" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	setOpenAIHeaders(w)
	w.WriteHeader(resp.StatusCode)
	copyResponseBody(w, resp.Body)
	return resp.StatusCode < 500
}

func copyResponseBody(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func copyProxyHeaders(dst, src http.Header) {
	for k, vals := range src {
		low := strings.ToLower(k)
		switch low {
		case "host", "content-length", "connection", "keep-alive", "transfer-encoding", "upgrade":
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
}

func setOpenAIHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	setOpenAIHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

func (s *AppState) handleChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages    []map[string]string `json:"messages"`
		Temperature float64             `json:"temperature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages required", 400)
		return
	}
	s.mu.Lock()
	cfg := s.config
	port := cfg.APIPort
	timeout := chatTimeoutDuration(cfg)
	s.mu.Unlock()
	body := chatCompletionBody(req.Messages, false, cfg)
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	b, _ := json.Marshal(body)
	client := &http.Client{Timeout: timeout}
	upReq, _ := http.NewRequestWithContext(r.Context(), "POST", "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions", strings.NewReader(string(b)))
	upReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(upReq)
	if err != nil {
		http.Error(w, "coordinator API unavailable: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		text := cleanLlamaError(string(msg))
		s.markChatComputeError(text)
		http.Error(w, text, resp.StatusCode)
		return
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	content := ""
	if len(out.Choices) > 0 {
		content = out.Choices[0].Message.Content
	}
	writeJSON(w, map[string]string{"content": content})
}

func (s *AppState) chatOnce(messages []map[string]string) (string, int, error) {
	if len(messages) == 0 {
		return "", 0, fmt.Errorf("messages required")
	}
	s.mu.Lock()
	cfg := s.config
	port := cfg.APIPort
	timeout := chatTimeoutDuration(cfg)
	if s.serverCmd != nil && s.serverCmd.Process != nil {
		s.serverLoad = "processing"
	}
	s.mu.Unlock()
	finalLoad := "ready"
	defer func() {
		s.mu.Lock()
		if s.serverCmd != nil && s.serverCmd.Process != nil && s.serverLoad == "processing" {
			s.serverLoad = finalLoad
		}
		s.mu.Unlock()
	}()
	client := &http.Client{Timeout: timeout}
	trimmed := false
	for {
		body := chatCompletionBody(messages, false, cfg)
		b, _ := json.Marshal(body)
		upReq, _ := http.NewRequest("POST", "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions", strings.NewReader(string(b)))
		upReq.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(upReq)
		if err != nil {
			finalLoad = "unreachable"
			return "", 0, fmt.Errorf("coordinator API unavailable: %w", err)
		}
		if resp.StatusCode >= 300 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			text := cleanLlamaError(string(msg))
			if isContextExceededError(text) {
				next := trimOldestChatMessage(messages)
				if len(next) < len(messages) {
					messages = next
					trimmed = true
					continue
				}
			}
			finalLoad = "error"
			s.markChatComputeError(text)
			return "", 0, fmt.Errorf("%s", text)
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			_ = resp.Body.Close()
			return "", 0, err
		}
		_ = resp.Body.Close()
		tokens := out.Usage.TotalTokens
		if tokens == 0 {
			tokens = out.Usage.PromptTokens + out.Usage.CompletionTokens
		}
		if trimmed {
			s.addLog("chat context exceeded server limit; trimmed oldest messages and retried")
		}
		if len(out.Choices) == 0 {
			return "", tokens, nil
		}
		return out.Choices[0].Message.Content, tokens, nil
	}
}

func defaultChatMaxTokens(cfg Config) int {
	if cfg.ChatMaxTokens > 0 {
		return cfg.ChatMaxTokens
	}
	return 1200
}

func chatCompletionBody(messages []map[string]string, stream bool, cfg Config) map[string]any {
	body := map[string]any{
		"model":          "local",
		"messages":       messages,
		"temperature":    0.35,
		"top_p":          0.9,
		"repeat_penalty": 1.12,
		"stop": []string{
			"<|im_end|>",
			"<|endoftext|>",
			"<|end_of_text|>",
			"</s>",
		},
		"stream": stream,
	}
	if !cfg.ChatNoTokenLimit {
		body["max_tokens"] = defaultChatMaxTokens(cfg)
	}
	return body
}

func (s *AppState) chatStream(messages []map[string]string, ch chan<- tuiChatStreamMsg) {
	defer close(ch)
	if len(messages) == 0 {
		ch <- tuiChatStreamMsg{Err: fmt.Errorf("messages required")}
		return
	}
	s.mu.Lock()
	cfg := s.config
	port := cfg.APIPort
	timeout := chatTimeoutDuration(cfg)
	if s.serverCmd != nil && s.serverCmd.Process != nil {
		s.serverLoad = "streaming"
	}
	s.mu.Unlock()
	finalLoad := "ready"
	defer func() {
		s.mu.Lock()
		if s.serverCmd != nil && s.serverCmd.Process != nil && (s.serverLoad == "streaming" || s.serverLoad == "processing") {
			s.serverLoad = finalLoad
		}
		s.mu.Unlock()
	}()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client := &http.Client{Timeout: timeout}
	trimmed := false
	var resp *http.Response
	for {
		body := chatCompletionBody(messages, true, cfg)
		b, _ := json.Marshal(body)
		upReq, _ := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions", strings.NewReader(string(b)))
		upReq.Header.Set("Content-Type", "application/json")
		var err error
		resp, err = client.Do(upReq)
		if err != nil {
			finalLoad = "unreachable"
			ch <- tuiChatStreamMsg{Err: fmt.Errorf("coordinator API unavailable: %w", err), Ms: time.Since(start).Milliseconds()}
			return
		}
		if resp.StatusCode < 300 {
			break
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		text := cleanLlamaError(string(msg))
		if isContextExceededError(text) {
			next := trimOldestChatMessage(messages)
			if len(next) < len(messages) {
				messages = next
				trimmed = true
				continue
			}
		}
		finalLoad = "error"
		s.markChatComputeError(text)
		ch <- tuiChatStreamMsg{Err: fmt.Errorf("%s", text), Ms: time.Since(start).Milliseconds()}
		return
	}
	defer resp.Body.Close()
	if trimmed {
		s.addLog("chat context exceeded server limit; trimmed oldest messages and retried")
	}
	tokens := 0
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		thought := delta.ReasoningContent
		if thought == "" {
			thought = delta.Reasoning
		}
		content := delta.Content
		if thought == "" && content == "" {
			continue
		}
		tokens++
		ch <- tuiChatStreamMsg{Content: content, Thought: thought, Tokens: tokens, Ms: time.Since(start).Milliseconds()}
	}
	if err := scanner.Err(); err != nil {
		finalLoad = "error"
		ch <- tuiChatStreamMsg{Err: err, Tokens: tokens, Ms: time.Since(start).Milliseconds()}
		return
	}
	ch <- tuiChatStreamMsg{Done: true, Tokens: tokens, Ms: time.Since(start).Milliseconds()}
}

func isContextExceededError(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "exceed_context") || strings.Contains(s, "exceeds the available context") || strings.Contains(s, "n_ctx")
}

func (s *AppState) markChatComputeError(text string) {
	s.mu.Lock()
	if s.serverCmd != nil && s.serverCmd.Process != nil {
		s.serverLoad = "error: " + short(cleanLlamaError(text), 160)
	}
	s.mu.Unlock()
}

func cleanLlamaError(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "model compute error"
	}
	var body struct {
		Error any `json:"error"`
	}
	if json.Unmarshal([]byte(text), &body) == nil && body.Error != nil {
		switch e := body.Error.(type) {
		case string:
			text = e
		case map[string]any:
			msg := strings.TrimSpace(fmt.Sprint(e["message"]))
			code := strings.TrimSpace(fmt.Sprint(e["code"]))
			if msg != "" && msg != "<nil>" {
				text = msg
			}
			if code != "" && code != "<nil>" {
				text = code + ": " + text
			}
		}
	}
	text = strings.TrimSpace(text)
	if strings.EqualFold(text, "Compute error") || strings.Contains(strings.ToLower(text), "compute error") {
		return "Compute error — model/runtime could not generate. Try lower context, batch/ubatch, GPU layers, or CPU mode."
	}
	return text
}

func trimOldestChatMessage(messages []map[string]string) []map[string]string {
	if len(messages) <= 2 {
		return messages
	}
	out := make([]map[string]string, 0, len(messages)-1)
	out = append(out, messages[0])
	out = append(out, messages[2:]...)
	return out
}

func (s *AppState) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages    []map[string]string `json:"messages"`
		Temperature float64             `json:"temperature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages required", 400)
		return
	}
	s.mu.Lock()
	cfg := s.config
	port := cfg.APIPort
	timeout := chatTimeoutDuration(cfg)
	s.mu.Unlock()
	body := chatCompletionBody(req.Messages, true, cfg)
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	b, _ := json.Marshal(body)
	client := &http.Client{Timeout: timeout}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	started := time.Now()
	send := func(kind string, v any) {
		bb, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, bb)
		if flusher != nil {
			flusher.Flush()
		}
	}
	var up *http.Response
	for {
		var err error
		upReq, _ := http.NewRequestWithContext(r.Context(), "POST", "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions", strings.NewReader(string(b)))
		upReq.Header.Set("Content-Type", "application/json")
		up, err = client.Do(upReq)
		if err != nil {
			send("error", map[string]string{"message": "coordinator API unavailable: " + err.Error()})
			return
		}
		if up.StatusCode < 300 {
			break
		}
		msg, _ := io.ReadAll(io.LimitReader(up.Body, 4096))
		_ = up.Body.Close()
		if up.StatusCode == 503 && strings.Contains(strings.ToLower(string(msg)), "loading model") && time.Since(started) < 3*time.Minute {
			send("status", map[string]any{"message": "Loading model…", "elapsedMs": time.Since(started).Milliseconds()})
			select {
			case <-r.Context().Done():
				return
			case <-time.After(1200 * time.Millisecond):
			}
			continue
		}
		send("error", map[string]string{"message": string(msg)})
		return
	}
	defer up.Body.Close()
	firstToken := time.Time{}
	tokens := 0
	scanner := bufio.NewScanner(up.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		thought := delta.ReasoningContent
		if thought == "" {
			thought = delta.Reasoning
		}
		content := delta.Content
		if thought == "" && content == "" {
			continue
		}
		if firstToken.IsZero() {
			firstToken = time.Now()
		}
		tokens++
		if thought != "" {
			send("thought", map[string]any{"content": thought, "tokens": tokens, "elapsedMs": time.Since(started).Milliseconds()})
		}
		if content != "" {
			send("token", map[string]any{"content": content, "tokens": tokens, "elapsedMs": time.Since(started).Milliseconds()})
		}
	}
	if err := scanner.Err(); err != nil {
		send("error", map[string]string{"message": err.Error()})
		return
	}
	stats := map[string]any{"elapsedMs": time.Since(started).Milliseconds(), "tokens": tokens}
	if !firstToken.IsZero() {
		stats["firstTokenMs"] = firstToken.Sub(started).Milliseconds()
	}
	send("done", stats)
}

func getJSON(u string, v any) error {
	client := &http.Client{Timeout: 20 * time.Second}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "ClusterKit/0.1")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (s *AppState) stop(kind string) {
	s.mu.Lock()
	var cmd *exec.Cmd
	if kind == "worker" {
		cmd = s.workerCmd
		s.workerCmd = nil
		s.workerStarted = time.Time{}
	} else {
		cmd = s.serverCmd
		s.serverCmd = nil
		s.serverContext = 0
		s.serverRPC = ""
	}
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		s.addLog("%s stopped", kind)
	}
}

func (s *AppState) stopWorkerManual() {
	s.mu.Lock()
	s.workerManualStopped = true
	s.mu.Unlock()
	s.stop("worker")
}

func (s *AppState) cleanupStaleLlamaProcess(kind string, port int) {
	if port <= 0 {
		return
	}
	pids := pidsListeningOnPort(port)
	if len(pids) == 0 {
		return
	}
	self := os.Getpid()
	killed := 0
	for _, pid := range pids {
		if pid <= 0 || pid == self {
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := proc.Kill(); err == nil {
			killed++
		}
	}
	if killed == 0 {
		return
	}
	s.addLog("preflight: stopped %d stale %s process(es) on port %d", killed, kind, port)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !checkTCP("127.0.0.1", port, 120*time.Millisecond) {
			return
		}
		time.Sleep(120 * time.Millisecond)
	}
}

func pidsListeningOnPort(port int) []int {
	switch runtime.GOOS {
	case "windows":
		return pidsListeningOnPortWindows(port)
	default:
		return pidsListeningOnPortUnix(port)
	}
}

func pidsListeningOnPortUnix(port int) []int {
	out, err := exec.Command("lsof", "-nP", "-tiTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		return nil
	}
	return parsePIDLines(string(out))
}

func pidsListeningOnPortWindows(port int) []int {
	ps := fmt.Sprintf(`Get-NetTCPConnection -LocalPort %d -State Listen -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess -Unique`, port)
	out, err := exec.Command("powershell", "-NoProfile", "-Command", ps).Output()
	if err != nil {
		return nil
	}
	return parsePIDLines(string(out))
}

func parsePIDLines(s string) []int {
	seen := map[int]bool{}
	pids := []int{}
	for _, field := range strings.Fields(s) {
		pid, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || pid <= 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		pids = append(pids, pid)
	}
	return pids
}

func (s *AppState) waitCmd(kind string, cmd *exec.Cmd, logFile *os.File) {
	err := cmd.Wait()
	if logFile != nil {
		_ = logFile.Close()
	}
	restartWorker := false
	s.mu.Lock()
	if kind == "worker" && s.workerCmd == cmd {
		s.workerCmd = nil
		s.workerStarting = false
		if !s.workerManualStopped {
			s.workerCrashCount++
			restartWorker = s.config.RoleExplicit && s.config.Role == "worker"
		}
	}
	if kind == "server" && s.serverCmd == cmd {
		s.serverCmd = nil
		s.serverStarting = false
		s.serverContext = 0
		s.serverRPC = ""
		if err != nil {
			s.serverCrashCount++
			s.serverLastCrash = time.Now()
		}
	}
	s.mu.Unlock()
	if err != nil {
		s.addLog("%s exited: %v", kind, err)
	} else {
		s.addLog("%s exited", kind)
	}
	if restartWorker {
		s.addLog("worker crashed; auto-restart disabled — start worker manually after checking logs")
	}
}

func (s *AppState) restartCoordinatorWithoutTensorSplit(cfg Config) {
	s.mu.Lock()
	if s.serverFallbackTried || s.serverCmd != nil || s.serverCrashCount >= 2 {
		s.mu.Unlock()
		return
	}
	s.serverFallbackTried = true
	s.mu.Unlock()
	s.addLog("coordinator crashed during startup; retrying once without --tensor-split")
	time.Sleep(1500 * time.Millisecond)
	cfg.TensorSplit = ""
	cfg.WeightedMode = false
	if cfg.Batch > 384 {
		cfg.Batch = 384
	}
	if cfg.UBatch > 192 {
		cfg.UBatch = 192
	}
	s.mu.Lock()
	s.config = cfg
	s.mu.Unlock()
	_ = saveConfig(cfg)
	if err := s.startCoordinatorProcess(cfg, false); err != nil {
		s.addLog("coordinator fallback failed: %v", err)
	}
}

func (s *AppState) workerHealthStats() (int, float64) {
	s.mu.Lock()
	crashes := s.workerCrashCount
	s.mu.Unlock()
	stability := 1.0
	if crashes > 0 {
		stability = 1.0 / (1.0 + float64(crashes)*0.35)
		if stability < 0.25 {
			stability = 0.25
		}
	}
	return crashes, stability
}

func (s *AppState) restartWorkerAfterCrash() {
	crashes, _ := s.workerHealthStats()
	delay := time.Duration(min(30, 2+crashes*3)) * time.Second
	s.addLog("worker crashed; auto-restart in %s (crashes=%d)", delay, crashes)
	time.Sleep(delay)
	s.mu.Lock()
	cfg := s.config
	running := s.workerCmd != nil
	manualStopped := s.workerManualStopped
	s.mu.Unlock()
	if !cfg.RoleExplicit || cfg.Role != "worker" || running || manualStopped {
		return
	}
	if !fileExists(llamaBin(cfg.LlamaDir, "rpc-server")) {
		s.addLog("worker auto-restart skipped: rpc-server not found")
		return
	}
	if err := s.startWorkerProcess(cfg); err != nil {
		s.addLog("worker auto-restart failed: %v", err)
	}
}

func (s *AppState) selfHealTick() {
	s.mu.Lock()
	if time.Since(s.lastSelfHeal) < 3*time.Second {
		s.mu.Unlock()
		return
	}
	s.lastSelfHeal = time.Now()
	cfg := s.config
	role := ""
	if cfg.RoleExplicit {
		role = strings.ToLower(cfg.Role)
	}
	workerRunning := s.workerCmd != nil && s.workerCmd.Process != nil
	workerManualStopped := s.workerManualStopped
	workerStarted := s.workerStarted
	serverRunning := s.serverCmd != nil && s.serverCmd.Process != nil
	serverLoad := s.serverLoad
	serverRPC := s.serverRPC
	lastCoordinatorHeal := s.lastCoordinatorHeal
	s.mu.Unlock()

	if role == "worker" {
		s.selfHealWorkerRPC(cfg, workerRunning, workerManualStopped, workerStarted)
		return
	}
	if role == "coordinator" && serverRunning {
		s.selfHealCoordinatorWorkers(cfg, serverLoad, serverRPC, lastCoordinatorHeal)
	}
}

func (s *AppState) selfHealWorkerRPC(cfg Config, running, manualStopped bool, started time.Time) {
	if manualStopped || !fileExists(llamaBin(cfg.LlamaDir, "rpc-server")) {
		return
	}
	if running {
		// Important: llama.cpp RPC can stop accepting quick TCP probes while the
		// coordinator is loading tensors into the remote device. Restarting a live
		// rpc-server during that window makes the coordinator fail with
		// "Remote RPC server crashed or returned malformed response".
		// If the process really dies, waitCmd handles the restart path.
		return
	}
	if checkTCP("127.0.0.1", cfg.RPCPort, 250*time.Millisecond) {
		return
	}
	s.addLog("self-heal: worker RPC process is not running; starting rpc-server")
	if err := s.startWorkerProcess(cfg); err != nil {
		s.addLog("self-heal worker start failed: %v", err)
	}
}

func (s *AppState) selfHealCoordinatorWorkers(cfg Config, serverLoad, serverRPC string, lastHeal time.Time) {
	if len(cfg.Workers) == 0 || time.Since(lastHeal) < 20*time.Second {
		return
	}
	status := statusFromServerLoad(serverLoad)
	if status == "loading" || status == "processing" {
		return
	}
	workers := s.classifyWorkers(append([]Worker(nil), cfg.Workers...), 450*time.Millisecond)
	cfg.Workers = workers
	desiredRPC := rpcList(policyOnlineWorkers(workers, cfg))
	s.mu.Lock()
	s.config = cfg
	s.mu.Unlock()
	_ = saveConfig(cfg)
	if desiredRPC == serverRPC {
		return
	}
	s.mu.Lock()
	if s.serverCmd == nil || s.serverCmd.Process == nil || s.serverLoad == "processing" || statusFromServerLoad(s.serverLoad) == "loading" {
		s.mu.Unlock()
		return
	}
	s.lastCoordinatorHeal = time.Now()
	s.mu.Unlock()
	s.addLog("self-heal: RPC worker set changed (%q -> %q); restarting coordinator", serverRPC, desiredRPC)
	s.stop("server")
	time.Sleep(1200 * time.Millisecond)
	if err := s.startCoordinatorProcess(cfg, true); err != nil {
		s.addLog("self-heal coordinator restart failed: %v", err)
	}
}

type logWriter struct {
	s      *AppState
	prefix string
}

func (lw logWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if lw.prefix == "server" {
		lw.s.noteServerLog(msg)
	}
	lw.s.addLog("[%s] %s", lw.prefix, msg)
	return len(p), nil
}

func logsDir() string { return filepath.Join(appDir(), "logs") }

func processLogPath(name string) string {
	_ = os.MkdirAll(logsDir(), 0755)
	stamp := time.Now().Format("20060102-150405")
	return filepath.Join(logsDir(), fmt.Sprintf("%s-%s.log", name, stamp))
}

func processLogWriter(name string) (*os.File, string) {
	path := processLogPath(name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, ""
	}
	_, _ = fmt.Fprintf(f, "ClusterKit %s log started %s\n", name, time.Now().Format(time.RFC3339))
	return f, path
}

func openLogTerminal(title, path string) {
	abs, _ := filepath.Abs(path)
	switch runtime.GOOS {
	case "darwin":
		cmdText := fmt.Sprintf("echo %s; echo %s; tail -n 200 -f %s", shellQuote(title), shellQuote(abs), shellQuote(abs))
		escaped := strings.ReplaceAll(strings.ReplaceAll(cmdText, `\`, `\\`), `"`, `\"`)
		_ = exec.Command("osascript", "-e", fmt.Sprintf(`tell application "Terminal" to do script "%s"`, escaped)).Start()
	case "windows":
		ps := fmt.Sprintf("Write-Host %q; Write-Host %q; if (!(Test-Path %q)) { New-Item -ItemType File -Path %q | Out-Null }; Get-Content -Path %q -Wait -Tail 200", title, abs, abs, abs, abs)
		_ = exec.Command("cmd", "/C", "start", title, "powershell", "-NoExit", "-Command", ps).Start()
	default:
		if term := os.Getenv("TERM_PROGRAM"); term != "" {
			_ = term
		}
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (s *AppState) noteServerLog(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "loading model"):
		s.serverLoad = "loading model"
		if s.loadStarted.IsZero() {
			s.loadStarted = time.Now()
		}
	case strings.Contains(low, "processing task") || strings.Contains(low, "slot launch_slot"):
		s.serverLoad = "processing"
	case strings.Contains(low, "all slots are idle"):
		s.serverLoad = "ready"
	case strings.Contains(low, "warming up"):
		s.serverLoad = "warming up"
	case strings.Contains(low, "initializing slots"):
		s.serverLoad = "initializing slots"
	case strings.Contains(low, "model loaded") || strings.Contains(low, "server is listening"):
		s.serverLoad = "ready"
		if s.loadReady.IsZero() {
			s.loadReady = time.Now()
		}
	case strings.Contains(low, "insufficient memory") || strings.Contains(low, "outofmemory") || strings.Contains(low, "compute error"):
		s.serverLoad = "metal/compute OOM — lower batch/ubatch"
	case strings.Contains(low, "exiting") || strings.Contains(low, "failed to load"):
		s.serverLoad = "error"
	}
}

func millisSince(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return time.Since(t).Milliseconds()
}

func llamaBin(dir, name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidates := []string{
		filepath.Join(dir, "build", "bin", name),
		filepath.Join(dir, "build", "bin", "Release", name),
		filepath.Join(dir, "bin", name),
		filepath.Join(dir, name),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	var found string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.EqualFold(d.Name(), name) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if found != "" {
		return found
	}
	return candidates[0]
}

func onlineWorkers(workers []Worker) []Worker {
	online := []Worker{}
	for _, wk := range workers {
		p := wk.Port
		if p == 0 {
			p = 50052
		}
		if checkTCP(wk.Host, p, 450*time.Millisecond) {
			wk.OK = true
			online = append(online, wk)
		}
	}
	return online
}

func stableOnlineWorkers(workers []Worker) []Worker {
	online := onlineWorkers(workers)
	stable := []Worker{}
	skipped := []string{}
	for _, wk := range online {
		backend := strings.ToLower(wk.Backend)
		// A crashed RPC worker aborts the whole llama-server load. 8GB Metal
		// workers are especially fragile, so skip repeatedly crashing workers by default.
		if wk.CrashCount >= 3 {
			skipped = append(skipped, fmt.Sprintf("%s(crash=%d)", firstNonEmpty(wk.Name, wk.Host), wk.CrashCount))
			continue
		}
		if strings.Contains(backend, "metal") && wk.RAMBytes > 0 && wk.RAMBytes <= 10*1024*1024*1024 && wk.CrashCount > 0 {
			skipped = append(skipped, fmt.Sprintf("%s(fragile-metal crash=%d)", firstNonEmpty(wk.Name, wk.Host), wk.CrashCount))
			continue
		}
		stable = append(stable, wk)
	}
	if len(skipped) > 0 {
		appendAppLog("skipping unstable RPC workers: " + strings.Join(skipped, ", "))
	}
	return stable
}

func policyOnlineWorkers(workers []Worker, cfg Config) []Worker {
	online := onlineWorkers(workers)
	filtered := make([]Worker, 0, len(online))
	skipped := []string{}
	manualLayers := cfg.CoordinatorLayers > 0 || manualWorkerLayers(online)
	for _, wk := range online {
		if wk.Disabled {
			skipped = append(skipped, firstNonEmpty(wk.Name, wk.Host))
			continue
		}
		if manualLayers && wk.Layers <= 0 {
			skipped = append(skipped, fmt.Sprintf("%s(layers=0)", firstNonEmpty(wk.Name, wk.Host)))
			continue
		}
		filtered = append(filtered, wk)
	}
	if len(skipped) > 0 {
		appendAppLog("skipping disabled RPC workers: " + strings.Join(skipped, ", "))
	}
	return filtered
}

func manualWorkerLayers(workers []Worker) bool {
	for _, wk := range workers {
		if !wk.Disabled && wk.Layers > 0 {
			return true
		}
	}
	return false
}

func manualLayerPlan(cfg Config) bool {
	return cfg.CoordinatorLayers > 0 || manualWorkerLayers(cfg.Workers)
}

func workerLayerTotal(workers []Worker) int {
	total := 0
	for _, wk := range workers {
		if !wk.Disabled && wk.Layers > 0 {
			total += wk.Layers
		}
	}
	return total
}

func manualLayerTotal(cfg Config, workers []Worker) int {
	total := workerLayerTotal(workers)
	if cfg.CoordinatorLayers > 0 {
		total += cfg.CoordinatorLayers
	}
	return total
}

func applyManualWorkerLayersToConfig(cfg Config) Config {
	if !manualLayerPlan(cfg) {
		return cfg
	}
	cfg.GPULayers = manualLayerTotal(cfg, cfg.Workers)
	cfg.SplitMode = "layer"
	cfg.CoordinatorLocal = cfg.CoordinatorLayers > 0
	return cfg
}

func clearManualWorkerLayers(cfg Config) Config {
	cfg.CoordinatorLayers = 0
	for i := range cfg.Workers {
		cfg.Workers[i].Layers = 0
		cfg.Workers[i].SplitWeight = 0
	}
	return cfg
}

func workerLayerPlanSummary(workers []Worker) string {
	if !manualWorkerLayers(workers) {
		return "off"
	}
	parts := []string{}
	for _, wk := range workers {
		if wk.Disabled || wk.Layers <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", short(firstNonEmpty(wk.Name, wk.Host), 12), wk.Layers))
	}
	return strings.Join(parts, ",")
}

func layerPlanSummary(cfg Config, workers []Worker) string {
	if !manualLayerPlan(cfg) && !manualWorkerLayers(workers) {
		return "off"
	}
	parts := []string{}
	if cfg.CoordinatorLayers > 0 {
		parts = append(parts, fmt.Sprintf("LOCAL=%d", cfg.CoordinatorLayers))
	}
	for _, wk := range workers {
		if wk.Disabled || wk.Layers <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", short(firstNonEmpty(wk.Name, wk.Host), 12), wk.Layers))
	}
	if len(parts) == 0 {
		return "off"
	}
	return strings.Join(parts, ",")
}

func workerLayerTensorSplit(workers []Worker) string {
	parts := []string{}
	for _, wk := range workers {
		if wk.Disabled || wk.Layers <= 0 {
			continue
		}
		parts = append(parts, strconv.Itoa(wk.Layers))
	}
	return strings.Join(parts, ",")
}

func layerTensorSplit(cfg Config, workers []Worker) string {
	parts := []string{}
	if cfg.CoordinatorLayers > 0 {
		parts = append(parts, strconv.Itoa(cfg.CoordinatorLayers))
	}
	if cfg.CoordinatorLayers == 0 && cfg.CoordinatorLocal {
		parts = append(parts, "0")
	}
	for _, wk := range workers {
		if wk.Disabled || wk.Layers <= 0 {
			continue
		}
		parts = append(parts, strconv.Itoa(wk.Layers))
	}
	return strings.Join(parts, ",")
}

func equalTensorSplit(workers []Worker) string {
	parts := []string{}
	for _, wk := range workers {
		if wk.Disabled {
			continue
		}
		parts = append(parts, "1")
	}
	return strings.Join(parts, ",")
}

func usableTensorSplit(workers []Worker) string {
	parts := []string{}
	for _, wk := range workers {
		if wk.Disabled {
			continue
		}
		gb := workerUsableGB(wk)
		if gb <= 0 {
			gb = 1
		}
		parts = append(parts, strconv.FormatFloat(gb, 'f', 1, 64))
	}
	return strings.Join(parts, ",")
}

func displayTensorSplit(cfg Config) string {
	if v := strings.TrimSpace(cfg.TensorSplit); v != "" {
		return v
	}
	if manualLayerPlan(cfg) {
		return "from layer plan: " + layerTensorSplit(cfg, cfg.Workers)
	}
	return "auto"
}

func nextTensorSplitPreset(cfg Config, dir int) string {
	presets := []string{""}
	if v := equalTensorSplit(cfg.Workers); v != "" {
		presets = appendUnique(presets, v)
	}
	if v := usableTensorSplit(cfg.Workers); v != "" {
		presets = appendUnique(presets, v)
	}
	if v := layerTensorSplit(cfg, cfg.Workers); v != "" {
		presets = appendUnique(presets, v)
	}
	current := strings.TrimSpace(cfg.TensorSplit)
	idx := 0
	for i, v := range presets {
		if v == current {
			idx = i
			break
		}
	}
	return presets[(idx+dir+len(presets))%len(presets)]
}

func appendUnique(vals []string, v string) []string {
	for _, existing := range vals {
		if existing == v {
			return vals
		}
	}
	return append(vals, v)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "unknown"
}

func rpcList(workers []Worker) string {
	parts := []string{}
	for _, wk := range workers {
		if wk.Host == "" {
			continue
		}
		p := wk.Port
		if p == 0 {
			p = 50052
		}
		parts = append(parts, fmt.Sprintf("%s:%d", wk.Host, p))
	}
	return strings.Join(parts, ",")
}

func rpcDeviceList(n int) string {
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf("RPC%d", i))
	}
	return strings.Join(parts, ",")
}

func coordinatorTensorSplit(cfg Config, workers []Worker) string {
	manual := strings.TrimSpace(cfg.TensorSplit)
	if len(workers) == 0 {
		return manual
	}
	if manualLayerPlan(cfg) {
		return layerTensorSplit(cfg, workers)
	}
	manualWorkerWeights := false
	manualLayers := manualWorkerLayers(workers)
	for _, wk := range workers {
		if wk.SplitWeight > 0 {
			manualWorkerWeights = true
			break
		}
	}
	// Per-worker weights map cleanly to RPC-only mode: every split entry belongs
	// to one RPC device. When the coordinator also computes locally, the tensor
	// split also needs a local-device entry, so keep using the global tensorSplit
	// field for that advanced/manual case.
	if cfg.CoordinatorLocal {
		return manual
	}
	if manualLayers {
		weights := make([]string, 0, len(workers))
		for _, wk := range workers {
			if wk.Layers <= 0 {
				continue
			}
			weights = append(weights, strconv.Itoa(wk.Layers))
		}
		return strings.Join(weights, ",")
	}
	if manualWorkerWeights {
		weights := make([]string, 0, len(workers))
		for _, wk := range workers {
			weight := wk.SplitWeight
			if weight <= 0 {
				weight = workerUsableGB(wk)
			}
			if weight <= 0 {
				weight = 1
			}
			weights = append(weights, strconv.FormatFloat(weight, 'f', 1, 64))
		}
		return strings.Join(weights, ",")
	}
	weights := splitTensorWeights(manual)
	if len(weights) == 0 {
		weights = make([]string, 0, len(workers))
		for _, wk := range workers {
			gb := workerUsableGB(wk)
			if gb <= 0 {
				gb = 1
			}
			weights = append(weights, strconv.FormatFloat(gb, 'f', 1, 64))
		}
	} else if len(weights) == len(workers)+1 {
		weights = weights[1:]
	}
	return strings.Join(weights, ",")
}

func splitTensorWeights(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func isAuxModelFile(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	return strings.HasPrefix(name, "mmproj-") || strings.Contains(name, "mmproj") || strings.Contains(name, "clip")
}

func localIP() string {
	ips := localIPs()
	if len(ips) > 0 {
		return ips[0]
	}
	return ""
}

func localIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	wifi := wifiInterfaceNames()
	type ipChoice struct {
		ip    string
		iface string
		score int
	}
	choices := []ipChoice{}
	seen := map[string]bool{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue
			}
			str := ip.String()
			if seen[str] {
				continue
			}
			seen[str] = true
			choices = append(choices, ipChoice{ip: str, iface: iface.Name, score: interfacePriority(iface.Name, wifi)})
		}
	}
	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].score != choices[j].score {
			return choices[i].score < choices[j].score
		}
		return choices[i].iface < choices[j].iface
	})
	ips := make([]string, 0, len(choices))
	for _, c := range choices {
		ips = append(ips, c.ip)
	}
	return ips
}

func interfacePriority(name string, wifi map[string]bool) int {
	low := strings.ToLower(name)
	// Direct cable/bridge links first. On macOS Thunderbolt Bridge is usually bridge0.
	if strings.Contains(low, "bridge") || strings.Contains(low, "thunderbolt") || strings.Contains(low, "usb") {
		return 0
	}
	// Wired ethernet before Wi‑Fi. On macOS both can be en*, so exclude known Wi‑Fi device names.
	if !wifi[name] && (strings.HasPrefix(low, "en") || strings.Contains(low, "ethernet")) {
		return 1
	}
	if wifi[name] || strings.Contains(low, "wi-fi") || strings.Contains(low, "wifi") || strings.Contains(low, "wlan") {
		return 2
	}
	// Link-local cable IPs are still useful for direct Mac↔Mac when no DHCP exists.
	return 3
}

func wifiInterfaceNames() map[string]bool {
	out := map[string]bool{}
	if runtime.GOOS == "darwin" {
		b, err := exec.Command("networksetup", "-listallhardwareports").Output()
		if err == nil {
			blocks := strings.Split(string(b), "\n\n")
			for _, block := range blocks {
				if !strings.Contains(strings.ToLower(block), "wi-fi") && !strings.Contains(strings.ToLower(block), "airport") {
					continue
				}
				for _, line := range strings.Split(block, "\n") {
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "Device:") {
						dev := strings.TrimSpace(strings.TrimPrefix(line, "Device:"))
						if dev != "" {
							out[dev] = true
						}
					}
				}
			}
		}
	}
	return out
}

func subnetBroadcasts() []string {
	out := []string{}
	for _, selfIP := range localIPs() {
		ip := net.ParseIP(selfIP).To4()
		if ip == nil {
			continue
		}
		out = append(out, net.IPv4(ip[0], ip[1], ip[2], 255).String())
	}
	return out
}

func checkTCP(host string, port int, timeout time.Duration) bool {
	if host == "" || port == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil && !errors.Is(err, exec.ErrNotFound) {
		appendAppLog("open browser failed: " + err.Error())
	}
}
