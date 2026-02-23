package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/UDL-TF/TourneyController/internal/config"
)

// Repository centralizes all database access for the controller.
type Repository struct {
	db *sql.DB
}

// New opens a PostgreSQL connection using the provided settings.
func New(cfg config.DatabaseConfig) (*Repository, error) {
	db, err := sql.Open("postgres", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("open postgres connection: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	} else {
		db.SetConnMaxLifetime(30 * time.Minute)
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &Repository{db: db}, nil
}

// Close closes the underlying sql.DB.
func (r *Repository) Close() error {
	if r.db == nil {
		return nil
	}
	return r.db.Close()
}

// Match mirrors a row in league_matches relevant to scheduling.
type Match struct {
	ID            int
	RosterAwayID  int
	RosterHomeID  int
	WinLimit      int
	Status        int
	ManualNotDone bool
}

// MatchRound mirrors league_match_rounds rows we care about.
type MatchRound struct {
	ID              int
	MatchID         int
	MapID           int
	HomeTeamScore   int
	AwayTeamScore   int
	LoserID         sql.NullInt64
	WinnerID        sql.NullInt64
	HasOutcome      bool
	ScoreDifference float64
	HomeReady       bool
	AwayReady       bool
}

// MatchDetails mirrors matches_server_details rows tracked by the site.
type MatchDetails struct {
	MatchID      int
	RoundID      int
	ServerIP     string
	Port         int
	SourceTVPort int
	Password     string
	Map          string
}

// League contains per-division gameplay metadata.
type League struct {
	MinPlayers           int
	MaxPlayers           int
	PointsPerRoundWin    float32
	PointsPerDraw        float32
	PointsPerRoundLoss   float32
	PointsPerMatchWin    float32
	PointsPerMatchLoss   float32
	PointsPerMatchDraw   float32
	PointsPerForfeitWin  float32
	PointsPerForfeitLoss float32
	PointsPerForfeitDraw float32
}

// Division captures the identifier and display name for a division.
type Division struct {
	ID   string
	Name string
}

// FetchMatches returns all matches whose status is in the provided set.
func (r *Repository) FetchMatches(ctx context.Context, statuses []int) ([]Match, error) {
	rows, err := r.db.QueryContext(ctx, `
        SELECT id, home_team_id, away_team_id, win_limit, status, manual_not_done
        FROM league_matches
        WHERE status = ANY($1) AND home_team_id IS NOT NULL AND away_team_id IS NOT NULL
    `, pq.Array(statuses))
	if err != nil {
		return nil, fmt.Errorf("query league_matches: %w", err)
	}
	defer rows.Close()

	var matches []Match
	for rows.Next() {
		var m Match
		if err := rows.Scan(&m.ID, &m.RosterHomeID, &m.RosterAwayID, &m.WinLimit, &m.Status, &m.ManualNotDone); err != nil {
			return nil, fmt.Errorf("scan league_match: %w", err)
		}
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate league_matches: %w", err)
	}
	return matches, nil
}

// FetchDivision returns the division metadata for a roster.
func (r *Repository) FetchDivision(ctx context.Context, rosterID int) (*Division, error) {
	var division Division
	if err := r.db.QueryRowContext(ctx, `
	        SELECT lr.division_id, ld.name
	        FROM league_rosters lr
	        JOIN league_divisions ld ON ld.id = lr.division_id
	        WHERE lr.id = $1
	    `, rosterID).Scan(&division.ID, &division.Name); err != nil {
		return nil, fmt.Errorf("fetch division for roster %d: %w", rosterID, err)
	}
	return &division, nil
}

// FetchLeague loads the League metadata by division ID.
func (r *Repository) FetchLeague(ctx context.Context, divisionID string) (*League, error) {
	var leagueID int
	if err := r.db.QueryRowContext(ctx, `
        SELECT league_id FROM league_divisions WHERE id = $1
    `, divisionID).Scan(&leagueID); err != nil {
		return nil, fmt.Errorf("fetch league_id for division %s: %w", divisionID, err)
	}

	league := &League{}
	if err := r.db.QueryRowContext(ctx, `
        SELECT min_players, max_players_in_game, points_per_round_win, points_per_round_draw, points_per_round_loss,
               points_per_match_win, points_per_match_loss, points_per_match_draw,
               points_per_forfeit_win, points_per_forfeit_loss, points_per_forfeit_draw
        FROM leagues
        WHERE id = $1
    `, leagueID).Scan(
		&league.MinPlayers,
		&league.MaxPlayers,
		&league.PointsPerRoundWin,
		&league.PointsPerDraw,
		&league.PointsPerRoundLoss,
		&league.PointsPerMatchWin,
		&league.PointsPerMatchLoss,
		&league.PointsPerMatchDraw,
		&league.PointsPerForfeitWin,
		&league.PointsPerForfeitLoss,
		&league.PointsPerForfeitDraw,
	); err != nil {
		return nil, fmt.Errorf("fetch league metadata %d: %w", leagueID, err)
	}

	return league, nil
}

// FetchTeamSteamIDs returns every SteamID on the roster as strings.
func (r *Repository) FetchTeamSteamIDs(ctx context.Context, rosterID int) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
        SELECT DISTINCT users.steam_id::text
        FROM league_roster_players lrp
        JOIN users ON users.id = lrp.user_id
        WHERE lrp.roster_id = $1
    `, rosterID)
	if err != nil {
		return nil, fmt.Errorf("fetch steam ids for roster %d: %w", rosterID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan steam id: %w", err)
		}
		if id.Valid && strings.TrimSpace(id.String) != "" {
			ids = append(ids, strings.TrimSpace(id.String))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate steam ids: %w", err)
	}
	return ids, nil
}

// FetchMatchRounds returns every round for a given match.
func (r *Repository) FetchMatchRounds(ctx context.Context, matchID int) ([]MatchRound, error) {
	rows, err := r.db.QueryContext(ctx, `
        SELECT id, match_id, map_id, home_team_score, away_team_score, loser_id, winner_id,
               has_outcome, score_difference, home_ready, away_ready
        FROM league_match_rounds
        WHERE match_id = $1
    `, matchID)
	if err != nil {
		return nil, fmt.Errorf("fetch match rounds for %d: %w", matchID, err)
	}
	defer rows.Close()

	var rounds []MatchRound
	for rows.Next() {
		var round MatchRound
		if err := rows.Scan(
			&round.ID,
			&round.MatchID,
			&round.MapID,
			&round.HomeTeamScore,
			&round.AwayTeamScore,
			&round.LoserID,
			&round.WinnerID,
			&round.HasOutcome,
			&round.ScoreDifference,
			&round.HomeReady,
			&round.AwayReady,
		); err != nil {
			return nil, fmt.Errorf("scan match round: %w", err)
		}
		rounds = append(rounds, round)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate match rounds: %w", err)
	}
	return rounds, nil
}

// FetchMapName returns the map name for the provided ID.
func (r *Repository) FetchMapName(ctx context.Context, mapID int) (string, error) {
	var mapName string
	if err := r.db.QueryRowContext(ctx, `SELECT name FROM maps WHERE id = $1`, mapID).Scan(&mapName); err != nil {
		return "", fmt.Errorf("fetch map %d: %w", mapID, err)
	}
	return mapName, nil
}

// FetchMatchDetails retrieves the saved connection details, if any.
func (r *Repository) FetchMatchDetails(ctx context.Context, matchID, roundID int) (*MatchDetails, error) {
	var details MatchDetails
	var portStr, sourceTVStr string
	err := r.db.QueryRowContext(ctx, `
        SELECT match_id, round_id, server_ip, port, sourcetvport, password, map
        FROM matches_server_details
        WHERE match_id = $1 AND round_id = $2
    `, matchID, roundID).Scan(
		&details.MatchID,
		&details.RoundID,
		&details.ServerIP,
		&portStr,
		&sourceTVStr,
		&details.Password,
		&details.Map,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("fetch match details (%d,%d): %w", matchID, roundID, err)
	}

	details.Port, err = strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil {
		return nil, fmt.Errorf("parse port from details: %w", err)
	}
	details.SourceTVPort, err = strconv.Atoi(strings.TrimSpace(sourceTVStr))
	if err != nil {
		return nil, fmt.Errorf("parse sourcetv port from details: %w", err)
	}
	return &details, nil
}

// UpsertMatchDetails inserts or updates the matches_server_details row.
func (r *Repository) UpsertMatchDetails(ctx context.Context, details MatchDetails) error {
	_, err := r.db.ExecContext(ctx, `
        INSERT INTO matches_server_details (match_id, server_ip, port, sourcetvport, password, map, round_id, created_at, updated_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
        ON CONFLICT (match_id, round_id)
        DO UPDATE SET server_ip = EXCLUDED.server_ip,
                      port = EXCLUDED.port,
                      sourcetvport = EXCLUDED.sourcetvport,
                      password = EXCLUDED.password,
                      map = EXCLUDED.map,
                      updated_at = NOW()
    `, details.MatchID, details.ServerIP, details.Port, details.SourceTVPort, details.Password, details.Map, details.RoundID)
	if err != nil {
		return fmt.Errorf("upsert match details: %w", err)
	}
	return nil
}

// DeleteMatchDetails removes the stored record once a server is torn down.
func (r *Repository) DeleteMatchDetails(ctx context.Context, matchID, roundID int) error {
	if _, err := r.db.ExecContext(ctx, `
        DELETE FROM matches_server_details WHERE match_id = $1 AND round_id = $2
    `, matchID, roundID); err != nil {
		return fmt.Errorf("delete match details (%d,%d): %w", matchID, roundID, err)
	}
	return nil
}

// SendNotificationsToTeams fans messages out to both rosters.
func (r *Repository) SendNotificationsToTeams(ctx context.Context, homeRosterID, awayRosterID int, message, link string) error {
	homeUsers, err := r.fetchTeamUserIDs(ctx, homeRosterID)
	if err != nil {
		return fmt.Errorf("fetch home user ids: %w", err)
	}
	awayUsers, err := r.fetchTeamUserIDs(ctx, awayRosterID)
	if err != nil {
		return fmt.Errorf("fetch away user ids: %w", err)
	}

	recipients := append(homeUsers, awayUsers...)
	for _, userID := range recipients {
		if err := r.createUserNotification(ctx, userID, message, link); err != nil {
			return fmt.Errorf("create notification for user %d: %w", userID, err)
		}
	}
	return nil
}

func (r *Repository) fetchTeamUserIDs(ctx context.Context, rosterID int) ([]int, error) {
	rows, err := r.db.QueryContext(ctx, `
        SELECT user_id FROM league_roster_players WHERE roster_id = $1
    `, rosterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *Repository) createUserNotification(ctx context.Context, userID int, message, link string) error {
	_, err := r.db.ExecContext(ctx, `
        INSERT INTO user_notifications (user_id, read, message, link, created_at, updated_at)
        VALUES ($1, FALSE, $2, $3, NOW(), NOW())
    `, userID, message, link)
	if err != nil {
		return fmt.Errorf("insert user_notification for %d: %w", userID, err)
	}
	return nil
}

// FetchMatchByID fetches a match by its ID
func (r *Repository) FetchMatchByID(ctx context.Context, matchID int) (*Match, error) {
	var match Match
	err := r.db.QueryRowContext(ctx, `
		SELECT id, home_team_id, away_team_id, win_limit, status, manual_not_done
		FROM league_matches
		WHERE id = $1
	`, matchID).Scan(&match.ID, &match.RosterHomeID, &match.RosterAwayID, &match.WinLimit, &match.Status, &match.ManualNotDone)
	
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("match with ID %d not found", matchID)
		}
		return nil, fmt.Errorf("fetch match %d: %w", matchID, err)
	}
	
	return &match, nil
}

// FetchMatchRoundByID fetches a specific round for a match
func (r *Repository) FetchMatchRoundByID(ctx context.Context, matchID, roundID int) (*MatchRound, error) {
	var round MatchRound
	err := r.db.QueryRowContext(ctx, `
		SELECT id, match_id, map_id, home_team_score, away_team_score, 
		       loser_squad_id, winner_squad_id, 
		       CASE WHEN loser_squad_id IS NOT NULL OR winner_squad_id IS NOT NULL THEN true ELSE false END,
		       COALESCE(home_team_score, 0) - COALESCE(away_team_score, 0),
		       home_ready, away_ready
		FROM league_match_rounds
		WHERE match_id = $1 AND id = $2
	`, matchID, roundID).Scan(&round.ID, &round.MatchID, &round.MapID, &round.HomeTeamScore, 
		&round.AwayTeamScore, &round.LoserID, &round.WinnerID, &round.HasOutcome, 
		&round.ScoreDifference, &round.HomeReady, &round.AwayReady)
	
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("round %d for match %d not found", roundID, matchID)
		}
		return nil, fmt.Errorf("fetch round %d for match %d: %w", roundID, matchID, err)
	}
	
	return &round, nil
}
