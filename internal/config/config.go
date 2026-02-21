package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config captures every tunable knob for the controller runtime.
type Config struct {
	Namespace     string
	PollInterval  time.Duration
	Chart         ChartConfig
	Database      DatabaseConfig
	Ports         PortsConfig
	SRCDS         SRCDSConfig
	Match         MatchConfig
	Networking    NetworkingConfig
	Notifications NotificationConfig
}

// ChartConfig controls how we render TF2Chart.
type ChartConfig struct {
	Path       string
	ValuesFile string
}

// DatabaseConfig feeds sql.Open and connection pool tuning.
type DatabaseConfig struct {
	Host            string
	Port            string
	User            string
	Password        string
	Name            string
	SSLMode         string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// DSN returns a lib/pq compatible connection string.
func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		d.Host,
		d.Port,
		d.User,
		d.Password,
		d.Name,
		d.SSLMode,
	)
}

// PortsConfig defines the discrete ranges used for each TF2 server port.
type PortsConfig struct {
	Game     PortRange
	SourceTV PortRange
	Client   PortRange
	Steam    PortRange
}

// PortRange represents an inclusive start/end block.
type PortRange struct {
	Start int
	End   int
}

// Validate ensures the range is well-formed.
func (r PortRange) Validate() error {
	if r.Start <= 0 || r.End <= 0 {
		return errors.New("ports must be positive integers")
	}
	if r.End < r.Start {
		return fmt.Errorf("invalid range %d-%d", r.Start, r.End)
	}
	return nil
}

// SRCDSConfig captures gameplay-related runtime settings.
type SRCDSConfig struct {
	TickRate           int
	MaxPlayersOverride int
	StaticToken        string
	PasswordLength     int
	RCONLength         int
}

// MatchConfig configures which matches should be reconciled.
type MatchConfig struct {
	TargetStatuses  []int
	DefaultMap      string
	DivisionFilters []string
}

// NetworkingConfig controls Kubernetes networking knobs.
type NetworkingConfig struct {
	HostNetwork           bool
	NodeIPPreference      NodeIPPreference
	ExternalTrafficPolicy string
}

// NodeIPPreference indicates whether we should prefer external or internal IPs.
type NodeIPPreference string

const (
	// NodeIPExternalFirst tries ExternalIP first, then falls back to InternalIP.
	NodeIPExternalFirst NodeIPPreference = "external-first"
	// NodeIPInternalOnly restricts discovery to InternalIP addresses.
	NodeIPInternalOnly NodeIPPreference = "internal-only"
)

// NotificationConfig controls optional user-facing alerts.
type NotificationConfig struct {
	Enabled    bool
	LinkFormat string
}

// Load parses environment variables into a strongly typed Config.
func Load() (*Config, error) {
	cfg := &Config{}

	cfg.Namespace = getEnv("NAMESPACE", "udl")

	intervalStr := getEnv("POLL_INTERVAL", "30s")
	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		return nil, fmt.Errorf("invalid POLL_INTERVAL: %w", err)
	}
	cfg.PollInterval = interval

	cfg.Chart = ChartConfig{
		Path:       getEnv("CHART_PATH", "oci://ghcr.io/udl-tf/charts/tf2chart"),
		ValuesFile: getEnv("CHART_VALUES_FILE", "./helm/values.yaml"),
	}

	db := DatabaseConfig{
		Host:     getEnv("DB_HOST", "postgres"),
		Port:     getEnv("DB_PORT", "5432"),
		User:     getEnv("DB_USER", "postgres"),
		Password: os.Getenv("DB_PASSWORD"),
		Name:     getEnv("DB_NAME", "udl"),
		SSLMode:  getEnv("DB_SSLMODE", "disable"),
	}

	maxOpen, err := getEnvInt("DB_MAX_OPEN_CONNS", 10)
	if err != nil {
		return nil, fmt.Errorf("invalid DB_MAX_OPEN_CONNS: %w", err)
	}
	db.MaxOpenConns = maxOpen

	maxIdle, err := getEnvInt("DB_MAX_IDLE_CONNS", 5)
	if err != nil {
		return nil, fmt.Errorf("invalid DB_MAX_IDLE_CONNS: %w", err)
	}
	db.MaxIdleConns = maxIdle

	lifetimeStr := getEnv("DB_CONN_MAX_LIFETIME", "0")
	if lifetimeStr != "" && lifetimeStr != "0" {
		lifetime, err := time.ParseDuration(lifetimeStr)
		if err != nil {
			return nil, fmt.Errorf("invalid DB_CONN_MAX_LIFETIME: %w", err)
		}
		db.ConnMaxLifetime = lifetime
	}
	cfg.Database = db

	ports, err := loadPortConfig()
	if err != nil {
		return nil, err
	}
	cfg.Ports = *ports

	tickRate, err := getEnvInt("SRCDS_TICKRATE", 128)
	if err != nil {
		return nil, fmt.Errorf("invalid SRCDS_TICKRATE: %w", err)
	}

	maxPlayersOverride, err := getEnvInt("SRCDS_MAX_PLAYERS_OVERRIDE", 0)
	if err != nil {
		return nil, fmt.Errorf("invalid SRCDS_MAX_PLAYERS_OVERRIDE: %w", err)
	}

	passwordLength, err := getEnvInt("SRCDS_PASSWORD_LENGTH", 10)
	if err != nil {
		return nil, fmt.Errorf("invalid SRCDS_PASSWORD_LENGTH: %w", err)
	}
	if passwordLength < 6 {
		return nil, errors.New("SRCDS_PASSWORD_LENGTH must be at least 6")
	}

	rconLength, err := getEnvInt("SRCDS_RCON_LENGTH", 46)
	if err != nil {
		return nil, fmt.Errorf("invalid SRCDS_RCON_LENGTH: %w", err)
	}
	if rconLength < 12 {
		return nil, errors.New("SRCDS_RCON_LENGTH must be at least 12")
	}

	cfg.SRCDS = SRCDSConfig{
		TickRate:           tickRate,
		MaxPlayersOverride: maxPlayersOverride,
		StaticToken:        os.Getenv("SRCDS_STATIC_TOKEN"),
		PasswordLength:     passwordLength,
		RCONLength:         rconLength,
	}

	statuses, err := parseIntSlice(getEnv("MATCH_STATUSES", "0"))
	if err != nil {
		return nil, fmt.Errorf("invalid MATCH_STATUSES: %w", err)
	}
	if len(statuses) == 0 {
		return nil, errors.New("MATCH_STATUSES must include at least one status code")
	}

	divisionFilters := parseStringSlice(getEnv("MATCH_DIVISION_FILTERS", ""))
	for i := range divisionFilters {
		divisionFilters[i] = strings.ToLower(divisionFilters[i])
	}

	cfg.Match = MatchConfig{
		TargetStatuses:  statuses,
		DefaultMap:      getEnv("DEFAULT_MAP", "tfdb_octagon_odb_a1"),
		DivisionFilters: divisionFilters,
	}

	hostNetwork, err := getEnvBool("HOST_NETWORK", false)
	if err != nil {
		return nil, fmt.Errorf("invalid HOST_NETWORK: %w", err)
	}

	nodePref := NodeIPPreference(strings.ToLower(getEnv("NODE_IP_PREFERENCE", string(NodeIPExternalFirst))))
	if nodePref != NodeIPExternalFirst && nodePref != NodeIPInternalOnly {
		return nil, fmt.Errorf("unsupported NODE_IP_PREFERENCE: %s", nodePref)
	}

	externalPolicy := getEnv("SERVICE_EXTERNAL_TRAFFIC_POLICY", "Cluster")

	cfg.Networking = NetworkingConfig{
		HostNetwork:           hostNetwork,
		NodeIPPreference:      nodePref,
		ExternalTrafficPolicy: externalPolicy,
	}

	notifyEnabled, err := getEnvBool("NOTIFICATIONS_ENABLED", true)
	if err != nil {
		return nil, fmt.Errorf("invalid NOTIFICATIONS_ENABLED: %w", err)
	}

	cfg.Notifications = NotificationConfig{
		Enabled:    notifyEnabled,
		LinkFormat: getEnv("NOTIFICATIONS_LINK_FORMAT", "/matches/%d"),
	}

	return cfg, nil
}

func loadPortConfig() (*PortsConfig, error) {
	game, err := parsePortRange(getEnv("PORT_RANGE_GAME", "30000-30299"))
	if err != nil {
		return nil, fmt.Errorf("invalid PORT_RANGE_GAME: %w", err)
	}

	sourceTV, err := parsePortRange(getEnv("PORT_RANGE_SOURCETV", "30300-30599"))
	if err != nil {
		return nil, fmt.Errorf("invalid PORT_RANGE_SOURCETV: %w", err)
	}

	client, err := parsePortRange(getEnv("PORT_RANGE_CLIENT", "40000-40299"))
	if err != nil {
		return nil, fmt.Errorf("invalid PORT_RANGE_CLIENT: %w", err)
	}

	steam, err := parsePortRange(getEnv("PORT_RANGE_STEAM", "29000-29299"))
	if err != nil {
		return nil, fmt.Errorf("invalid PORT_RANGE_STEAM: %w", err)
	}

	return &PortsConfig{Game: game, SourceTV: sourceTV, Client: client, Steam: steam}, nil
}

func parsePortRange(raw string) (PortRange, error) {
	parts := strings.Split(strings.TrimSpace(raw), "-")
	if len(parts) != 2 {
		return PortRange{}, fmt.Errorf("expected format start-end, got %q", raw)
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return PortRange{}, err
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return PortRange{}, err
	}
	r := PortRange{Start: start, End: end}
	if err := r.Validate(); err != nil {
		return PortRange{}, err
	}
	return r, nil
}

func getEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func getEnvInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func getEnvBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, err
	}
	return value, nil
}

func parseIntSlice(raw string) ([]int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []int{}, nil
	}
	parts := strings.Split(trimmed, ",")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		num, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		out = append(out, num)
	}
	return out, nil
}

func parseStringSlice(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []string{}
	}
	parts := strings.Split(trimmed, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
