package controller

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/chartutil"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/UDL-TF/TourneyController/internal/chart"
	"github.com/UDL-TF/TourneyController/internal/config"
	"github.com/UDL-TF/TourneyController/internal/database"
	"github.com/UDL-TF/TourneyController/internal/ports"
	"github.com/UDL-TF/TourneyController/internal/steam"
)

// Controller coordinates database polling with Kubernetes reconciliation.
type Controller struct {
	cfg           *config.Config
	repo          *database.Repository
	clientset     kubernetes.Interface
	portAllocator *ports.Allocator
	renderer      *chart.Renderer
	steamClient   *steam.SteamClient
}

// New wires together the reconciliation dependencies.
func New(cfg *config.Config, repo *database.Repository, clientset kubernetes.Interface, renderer *chart.Renderer) *Controller {
	var steamClient *steam.SteamClient
	if cfg.Steam.EnableAutoTokens && cfg.Steam.APIKey != "" {
		steamClient = steam.NewSteamClient(cfg.Steam.APIKey)
	}

	return &Controller{
		cfg:           cfg,
		repo:          repo,
		clientset:     clientset,
		portAllocator: ports.NewAllocator(cfg.Ports),
		renderer:      renderer,
		steamClient:   steamClient,
	}
}

// Run blocks until the context is cancelled, reconciling on every tick.
func (c *Controller) Run(ctx context.Context) error {
	klog.Info("controller started")
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	if err := c.reconcile(ctx); err != nil {
		klog.Errorf("initial reconcile failed: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			klog.Info("controller shutting down")
			return ctx.Err()
		case <-ticker.C:
			if err := c.reconcile(ctx); err != nil {
				klog.Errorf("reconcile tick failed: %v", err)
			}
		}
	}
}

func (c *Controller) reconcile(ctx context.Context) error {
	matches, err := c.repo.FetchMatches(ctx, c.cfg.Match.TargetStatuses)
	if err != nil {
		return err
	}

	for _, match := range matches {
		if err := c.reconcileMatch(ctx, match); err != nil {
			klog.Errorf("match %d reconcile error: %v", match.ID, err)
		}
	}
	return nil
}

func (c *Controller) reconcileMatch(ctx context.Context, match database.Match) error {
	division, err := c.repo.FetchDivision(ctx, match.RosterHomeID)
	if err != nil {
		return fmt.Errorf("fetch division: %w", err)
	}

	if !c.divisionMatchesFilter(division.Name) {
		klog.V(2).Infof("skipping match %d: division %q excluded by filter", match.ID, division.Name)
		return nil
	}

	league, err := c.repo.FetchLeague(ctx, division.ID)
	if err != nil {
		return fmt.Errorf("fetch league: %w", err)
	}

	homeIDs, err := c.repo.FetchTeamSteamIDs(ctx, match.RosterHomeID)
	if err != nil {
		return fmt.Errorf("fetch home steam ids: %w", err)
	}

	awayIDs, err := c.repo.FetchTeamSteamIDs(ctx, match.RosterAwayID)
	if err != nil {
		return fmt.Errorf("fetch away steam ids: %w", err)
	}

	rounds, err := c.repo.FetchMatchRounds(ctx, match.ID)
	if err != nil {
		return fmt.Errorf("fetch match rounds: %w", err)
	}

	for _, round := range rounds {
		mapName, err := c.repo.FetchMapName(ctx, round.MapID)
		if err != nil {
			klog.Warningf("round %d map lookup failed, using default: %v", round.ID, err)
			mapName = c.cfg.Match.DefaultMap
		}

		details, err := c.repo.FetchMatchDetails(ctx, match.ID, round.ID)
		if err != nil {
			return fmt.Errorf("fetch match details: %w", err)
		}

		needsServer := match.ManualNotDone || !round.HasOutcome
		releaseName := releaseName(match.ID, round.ID)

		if needsServer {
			if err := c.ensureRound(ctx, match, round, division.ID, league, homeIDs, awayIDs, mapName, details, releaseName); err != nil {
				klog.Errorf("ensure round %d: %v", round.ID, err)
			}
			continue
		}

		if details != nil {
			if err := c.teardownRound(ctx, match, round, division.ID, league, homeIDs, awayIDs, mapName, releaseName, details); err != nil {
				klog.Errorf("teardown round %d: %v", round.ID, err)
			}
		}
	}

	return nil
}

func (c *Controller) ensureRound(
	ctx context.Context,
	match database.Match,
	round database.MatchRound,
	divisionID string,
	league *database.League,
	homeIDs, awayIDs []string,
	mapName string,
	details *database.MatchDetails,
	releaseName string,
) error {
	state, err := c.loadServerState(ctx, releaseName)
	if err != nil {
		return fmt.Errorf("load server state: %w", err)
	}

	isNew := false
	if state == nil {
		assign, err := c.portAllocator.Allocate(ctx, c.clientset.CoreV1().Services(c.cfg.Namespace))
		if err != nil {
			return fmt.Errorf("allocate ports: %w", err)
		}
		password, err := generateSecret(c.cfg.SRCDS.PasswordLength)
		if err != nil {
			return fmt.Errorf("generate password: %w", err)
		}
		rcon, err := generateSecret(c.cfg.SRCDS.RCONLength)
		if err != nil {
			return fmt.Errorf("generate rcon: %w", err)
		}

		token, err := c.generateSRCDSToken(match.ID, round.ID)
		if err != nil {
			klog.Warningf("failed to generate SRCDS token: %v, falling back to static token", err)
			token = c.cfg.SRCDS.StaticToken
		}

		state = &serverState{
			ReleaseName: releaseName,
			Ports:       assign,
			Password:    password,
			RCON:        rcon,
			Map:         mapName,
			Token:       token,
		}
		isNew = true
	} else {
		state.Map = preferValue(mapName, state.Map, c.cfg.Match.DefaultMap)
		if state.Token == "" {
			token, err := c.generateSRCDSToken(match.ID, round.ID)
			if err != nil {
				klog.Warningf("failed to generate SRCDS token for existing server: %v, falling back to static token", err)
				state.Token = c.cfg.SRCDS.StaticToken
			} else {
				state.Token = token
			}
		}
	}

	if err := c.persistStateSecret(ctx, match, round, state); err != nil {
		return fmt.Errorf("persist secret: %w", err)
	}

	values := c.buildValues(match, round, divisionID, league, homeIDs, awayIDs, state)
	if err := c.applyHelmRelease(ctx, releaseName, values); err != nil {
		return fmt.Errorf("apply helm release: %w", err)
	}

	nodeIP, err := c.pickNodeIP(ctx)
	if err != nil {
		return fmt.Errorf("discover node ip: %w", err)
	}

	detailsPayload := database.MatchDetails{
		MatchID:      match.ID,
		RoundID:      round.ID,
		ServerIP:     nodeIP,
		Port:         state.Ports.Game,
		SourceTVPort: state.Ports.SourceTV,
		Password:     state.Password,
		Map:          preferValue(state.Map, mapName, c.cfg.Match.DefaultMap),
	}

	if err := c.repo.UpsertMatchDetails(ctx, detailsPayload); err != nil {
		return fmt.Errorf("upsert match details: %w", err)
	}

	if isNew && c.cfg.Notifications.Enabled {
		message := fmt.Sprintf("Match %d Round %d is running on %s:%d with password %s", match.ID, round.ID, nodeIP, state.Ports.Game, state.Password)
		link := fmt.Sprintf(c.cfg.Notifications.LinkFormat, match.ID)
		if err := c.repo.SendNotificationsToTeams(ctx, match.RosterHomeID, match.RosterAwayID, message, link); err != nil {
			klog.Errorf("notifications failed for match %d: %v", match.ID, err)
		}
	}

	return nil
}

func (c *Controller) teardownRound(
	ctx context.Context,
	match database.Match,
	round database.MatchRound,
	divisionID string,
	league *database.League,
	homeIDs, awayIDs []string,
	mapName, releaseName string,
	details *database.MatchDetails,
) error {
	state, err := c.loadServerState(ctx, releaseName)
	if err != nil {
		return fmt.Errorf("load state for teardown: %w", err)
	}
	if state == nil {
		state = &serverState{
			ReleaseName: releaseName,
			Ports: ports.Assignment{
				Game:     details.Port,
				SourceTV: details.SourceTVPort,
				Client:   details.Port + 1,
				Steam:    details.Port + 2,
			},
			Password: details.Password,
			RCON:     "",
			Map:      preferValue(details.Map, mapName, c.cfg.Match.DefaultMap),
			Token:    c.cfg.SRCDS.StaticToken,
		}
	}

	if err := c.deleteHelmRelease(ctx, releaseName, c.buildValues(match, round, divisionID, league, homeIDs, awayIDs, state)); err != nil {
		return fmt.Errorf("delete helm release: %w", err)
	}

	if err := c.repo.DeleteMatchDetails(ctx, match.ID, round.ID); err != nil {
		return fmt.Errorf("delete match details: %w", err)
	}

	if err := c.deleteStateSecret(ctx, releaseName); err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}

	// Clean up Steam token if enabled
	if err := c.cleanupSRCDSToken(match.ID, round.ID); err != nil {
		klog.Warningf("failed to cleanup SRCDS token for match %d round %d: %v", match.ID, round.ID, err)
	}

	klog.Infof("tore down server for match %d round %d", match.ID, round.ID)
	return nil
}

func (c *Controller) buildValues(
	match database.Match,
	round database.MatchRound,
	divisionID string,
	league *database.League,
	homeIDs, awayIDs []string,
	state *serverState,
) chartutil.Values {
	maxPlayers := league.MaxPlayers
	if c.cfg.SRCDS.MaxPlayersOverride > 0 {
		maxPlayers = c.cfg.SRCDS.MaxPlayersOverride
	}

	env := []map[string]interface{}{
		envVar("SRCDS_PORT", state.Ports.Game),
		envVar("SRCDS_PW", state.Password),
		envVar("SRCDS_MAXPLAYERS", maxPlayers),
		envVar("SRCDS_TICKRATE", c.cfg.SRCDS.TickRate),
		envVar("SRCDS_RCONPW", state.RCON),
		envVar("SRCDS_STARTMAP", preferValue(state.Map, c.cfg.Match.DefaultMap, "")),
		envVar("SRCDS_STATIC_HOSTNAME", fmt.Sprintf("UDL.TF | %d | Round #%d", match.ID, round.ID)),
		envVar("SRCDS_TOKEN", state.Token),
		envVar("SRCDS_TV_PORT", state.Ports.SourceTV),
		envVar("SRCDS_CLIENT_PORT", state.Ports.Client),
		envVar("SRCDS_STEAM_PORT", state.Ports.Steam),
		envVar("MATCH_ID", match.ID),
		envVar("ROUND_ID", round.ID),
		envVar("AWAY_TEAM", strings.Join(awayIDs, ",")),
		envVar("AWAY_TEAM_ID", match.RosterAwayID),
		envVar("HOME_TEAM", strings.Join(homeIDs, ",")),
		envVar("HOME_TEAM_ID", match.RosterHomeID),
		envVar("MIN_PLAYERS", league.MinPlayers),
		envVar("MAX_PLAYERS", maxPlayers),
		envVar("WIN_LIMIT", match.WinLimit),
	}

	appPorts := []map[string]interface{}{
		namedPort("game-udp", state.Ports.Game, "UDP", 0),
		namedPort("game-tcp", state.Ports.Game, "TCP", 0),
		namedPort("sourcetv", state.Ports.SourceTV, "UDP", 0),
		namedPort("client", state.Ports.Client, "UDP", 0),
		namedPort("steam", state.Ports.Steam, "UDP", 0),
	}

	servicePorts := []map[string]interface{}{
		servicePort("game-udp", state.Ports.Game, state.Ports.Game, "UDP"),
		servicePort("game-tcp", state.Ports.Game, state.Ports.Game, "TCP"),
		servicePort("sourcetv", state.Ports.SourceTV, state.Ports.SourceTV, "UDP"),
		servicePort("client", state.Ports.Client, state.Ports.Client, "UDP"),
		servicePort("steam", state.Ports.Steam, state.Ports.Steam, "UDP"),
	}

	serviceConfig := map[string]interface{}{
		"enabled": !c.cfg.Networking.HostNetwork,
	}
	if serviceConfig["enabled"].(bool) {
		serviceConfig["type"] = "NodePort"
		serviceConfig["nameOverride"] = state.ReleaseName
		serviceConfig["ports"] = servicePorts
	}

	values := chartutil.Values{
		"workload": map[string]interface{}{
			"kind":               "Deployment",
			"nameOverride":       state.ReleaseName,
			"deploymentStrategy": map[string]interface{}{"type": "Recreate"},
		},
		"service": serviceConfig,
		"app": map[string]interface{}{
			"name":          state.ReleaseName,
			"containerPort": state.Ports.Game,
			"ports":         appPorts,
			"env":           env,
			"stdin":         true,
			"tty":           true,
		},
		"paths": map[string]interface{}{
			"hostSource":      "/mnt/tf2",
			"hostPathType":    "Directory",
			"containerTarget": "/tf",
		},
		"decompressor": map[string]interface{}{
			"scanBase":     false,
			"scanOverlays": []string{"serverfiles-dodgeball-tourney"},
			"cache": map[string]interface{}{
				"enabled":        true,
				"type":           "hostPath",
				"mountAsOverlay": true,
				"overlayName":    "decomp-cache",
				"hostPath":       "/mnt/dodgeball-cache",
				"hostPathType":   "DirectoryOrCreate",
			},
		},
		"writablePaths": []string{
			"tf/logs",
			"tf/demos",
			"tf/addons/sourcemod/data",
			"tf/addons/sourcemod/logs",
		},
		"copyTemplates": []map[string]interface{}{
			{
				"targetPath":  "tf/addons/sourcemod/configs/sourcebans",
				"overlay":     "serverfiles-base",
				"sourcePath":  "serverfiles/base/addons/sourcemod/configs/sourcebans",
				"cleanTarget": false,
				"targetMode":  "writable",
				"onlyOnInit":  true,
			},
		},
		"overlays": []map[string]interface{}{
			{
				"name":         "serverfiles-base-sourcemod",
				"path":         "/mnt/serverfiles",
				"sourcePath":   "serverfiles/base/sourcemod",
				"hostPathType": "Directory",
				"readOnly":     false,
			},
			{
				"name":         "serverfiles-base-sourcebans",
				"path":         "/mnt/serverfiles",
				"sourcePath":   "serverfiles/base/sourcebans",
				"hostPathType": "Directory",
				"readOnly":     false,
			},
			{
				"name":         "serverfilesprivate-base",
				"path":         "/mnt/serverfilesprivate",
				"sourcePath":   "serverfiles/base",
				"hostPathType": "Directory",
				"readOnly":     false,
			},
			{
				"name":         "serverfilesprivate-dodgeball-base",
				"path":         "/mnt/serverfilesprivate",
				"sourcePath":   "serverfiles/dodgeball/base",
				"hostPathType": "Directory",
				"readOnly":     false,
			},
			{
				"name":         "serverfiles-dodgeball-tourney",
				"path":         "/mnt/serverfiles",
				"sourcePath":   "serverfiles/dodgeball/tourney",
				"hostPathType": "Directory",
				"readOnly":     false,
			},
		},
		"permissionsInit": map[string]interface{}{
			"applyDuringMerge": true,
			"applyPaths":       []string{"/tf"},
			"user":             1000,
			"group":            1000,
			"chmod":            "775",
		},
		"podLabels": map[string]interface{}{
			"udl.tf/match-id": strconv.Itoa(match.ID),
			"udl.tf/round-id": strconv.Itoa(round.ID),
			"udl.tf/division": divisionID,
		},
	}

	if c.cfg.Networking.HostNetwork {
		values["hostNetwork"] = true
		values["dnsPolicy"] = "ClusterFirstWithHostNet"
	} else if c.cfg.Networking.ExternalTrafficPolicy != "" {
		service := values["service"].(map[string]interface{})
		service["externalTrafficPolicy"] = c.cfg.Networking.ExternalTrafficPolicy
	}

	return values
}

func (c *Controller) divisionMatchesFilter(name string) bool {
	filters := c.cfg.Match.DivisionFilters
	if len(filters) == 0 {
		return true
	}
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "" {
		return false
	}
	for _, filter := range filters {
		if filter == "" {
			continue
		}
		if strings.Contains(normalized, filter) {
			return true
		}
	}
	return false
}

func (c *Controller) applyHelmRelease(ctx context.Context, releaseName string, overrides chartutil.Values) error {
	if c.renderer == nil {
		return fmt.Errorf("helm renderer is not configured")
	}
	return c.renderer.Apply(ctx, releaseName, overrides)
}

func (c *Controller) deleteHelmRelease(ctx context.Context, releaseName string, overrides chartutil.Values) error {
	if c.renderer == nil {
		return fmt.Errorf("helm renderer is not configured")
	}
	return c.renderer.Delete(ctx, releaseName, overrides)
}

func envVar(name string, value interface{}) map[string]interface{} {
	return map[string]interface{}{
		"name":  name,
		"value": fmt.Sprintf("%v", value),
	}
}

func namedPort(name string, port int, protocol string, hostPort int) map[string]interface{} {
	entry := map[string]interface{}{
		"name":          name,
		"containerPort": port,
		"protocol":      protocol,
	}
	if hostPort > 0 {
		entry["hostPort"] = hostPort
	}
	return entry
}

func servicePort(name string, port, target int, protocol string) map[string]interface{} {
	return map[string]interface{}{
		"name":       name,
		"port":       port,
		"targetPort": target,
		"protocol":   protocol,
		"nodePort":   port,
	}
}

func (c *Controller) pickNodeIP(ctx context.Context) (string, error) {
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	var internalCandidate string
	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeExternalIP && isIPv4(addr.Address) && c.cfg.Networking.NodeIPPreference == config.NodeIPExternalFirst {
				return addr.Address, nil
			}
			if addr.Type == corev1.NodeInternalIP && isIPv4(addr.Address) && internalCandidate == "" {
				internalCandidate = addr.Address
			}
		}
	}
	if internalCandidate != "" {
		return internalCandidate, nil
	}
	return "", fmt.Errorf("no suitable node IP found")
}

func isIPv4(addr string) bool {
	ip := net.ParseIP(strings.TrimSpace(addr))
	return ip != nil && ip.To4() != nil
}

func releaseName(matchID, roundID int) string {
	return fmt.Sprintf("udl-%d-r%d", matchID, roundID)
}

func (c *Controller) loadServerState(ctx context.Context, releaseName string) (*serverState, error) {
	secretName := c.secretName(releaseName)
	secret, err := c.clientset.CoreV1().Secrets(c.cfg.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	parse := func(key string) string {
		if data, ok := secret.Data[key]; ok {
			return string(data)
		}
		return ""
	}

	toInt := func(key string) (int, error) {
		raw := parse(key)
		if raw == "" {
			return 0, fmt.Errorf("secret missing %s", key)
		}
		return strconv.Atoi(raw)
	}

	gamePort, err := toInt(secretKeyGamePort)
	if err != nil {
		return nil, err
	}
	sourcePort, err := toInt(secretKeySourcePort)
	if err != nil {
		return nil, err
	}
	clientPort, err := toInt(secretKeyClientPort)
	if err != nil {
		return nil, err
	}
	steamPort, err := toInt(secretKeySteamPort)
	if err != nil {
		return nil, err
	}

	state := &serverState{
		ReleaseName: releaseName,
		Ports: ports.Assignment{
			Game:     gamePort,
			SourceTV: sourcePort,
			Client:   clientPort,
			Steam:    steamPort,
		},
		Password: parse(secretKeyPassword),
		RCON:     parse(secretKeyRCON),
		Map:      parse(secretKeyMap),
		Token:    parse(secretKeyToken),
	}
	return state, nil
}

func (c *Controller) persistStateSecret(ctx context.Context, match database.Match, round database.MatchRound, state *serverState) error {
	secretName := c.secretName(state.ReleaseName)
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: c.cfg.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/instance": state.ReleaseName,
				"udl.tf/match-id":            strconv.Itoa(match.ID),
				"udl.tf/round-id":            strconv.Itoa(round.ID),
			},
		},
		Data: map[string][]byte{
			secretKeyPassword:   []byte(state.Password),
			secretKeyRCON:       []byte(state.RCON),
			secretKeyGamePort:   []byte(strconv.Itoa(state.Ports.Game)),
			secretKeySourcePort: []byte(strconv.Itoa(state.Ports.SourceTV)),
			secretKeyClientPort: []byte(strconv.Itoa(state.Ports.Client)),
			secretKeySteamPort:  []byte(strconv.Itoa(state.Ports.Steam)),
			secretKeyMap:        []byte(preferValue(state.Map, c.cfg.Match.DefaultMap, "")),
			secretKeyToken:      []byte(state.Token),
		},
		Type: corev1.SecretTypeOpaque,
	}

	secrets := c.clientset.CoreV1().Secrets(c.cfg.Namespace)
	existing, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			_, err = secrets.Create(ctx, desired, metav1.CreateOptions{})
		}
		return err
	}

	desired.ResourceVersion = existing.ResourceVersion
	_, err = secrets.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

func (c *Controller) deleteStateSecret(ctx context.Context, releaseName string) error {
	secrets := c.clientset.CoreV1().Secrets(c.cfg.Namespace)
	if err := secrets.Delete(ctx, c.secretName(releaseName), metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (c *Controller) secretName(releaseName string) string {
	return fmt.Sprintf("%s-settings", releaseName)
}

func generateSecret(length int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	output := make([]byte, length)
	for i := range output {
		idxBig, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		output[i] = alphabet[idxBig.Int64()]
	}
	return string(output), nil
}

func preferValue(primary string, fallbacks ...string) string {
	candidates := append([]string{primary}, fallbacks...)
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// generateSRCDSToken creates a new SRCDS token using Steam Web API if configured,
// otherwise falls back to the static token.
func (c *Controller) generateSRCDSToken(matchID int, roundID int) (string, error) {
	// If auto token generation is disabled or no Steam client, use static token
	if !c.cfg.Steam.EnableAutoTokens || c.steamClient == nil {
		return c.cfg.SRCDS.StaticToken, nil
	}

	// Generate memo for the token using the template
	memo := fmt.Sprintf(c.cfg.Steam.TokenMemoTemplate, matchID, roundID)

	// Create a new Steam account for this server
	account, err := c.steamClient.CreateAccount(c.cfg.Steam.AppID, memo)
	if err != nil {
		return "", fmt.Errorf("create steam account: %w", err)
	}

	klog.V(2).Infof("created SRCDS token for match %d round %d: steamid=%s", matchID, roundID, account.SteamID)

	return account.LoginToken, nil
}

// cleanupSRCDSToken attempts to delete the Steam account associated with a match/round
// if token cleanup is enabled.
func (c *Controller) cleanupSRCDSToken(matchID int, roundID int) error {
	// If token cleanup is disabled or no Steam client, nothing to do
	if !c.cfg.Steam.EnableTokenCleanup || c.steamClient == nil {
		return nil
	}

	// Generate memo pattern to search for
	memo := fmt.Sprintf(c.cfg.Steam.TokenMemoTemplate, matchID, roundID)

	// Get all Steam accounts
	accounts, err := c.steamClient.GetAccountList()
	if err != nil {
		return fmt.Errorf("get account list: %w", err)
	}

	// Find and delete accounts with matching memo
	for _, account := range accounts {
		if account.Memo == memo && !account.IsDeleted {
			if err := c.steamClient.DeleteAccount(account.SteamID); err != nil {
				klog.Warningf("failed to delete Steam account %s: %v", account.SteamID, err)
			} else {
				klog.V(2).Infof("deleted Steam account %s for match %d round %d", account.SteamID, matchID, roundID)
			}
		}
	}

	return nil
}

type serverState struct {
	ReleaseName string
	Ports       ports.Assignment
	Password    string
	RCON        string
	Map         string
	Token       string
}

const (
	secretKeyPassword   = "password"
	secretKeyRCON       = "rcon"
	secretKeyGamePort   = "game_port"
	secretKeySourcePort = "sourcetv_port"
	secretKeyClientPort = "client_port"
	secretKeySteamPort  = "steam_port"
	secretKeyMap        = "map"
	secretKeyToken      = "token"
)
