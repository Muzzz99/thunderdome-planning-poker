package poker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/StevenWeathers/thunderdome-planning-poker/internal/db"

	"github.com/StevenWeathers/thunderdome-planning-poker/thunderdome"
	"github.com/microcosm-cc/bluemonday"
	"github.com/uptrace/opentelemetry-go-extra/otelzap"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Service represents the poker database service
type Service struct {
	DB                  *sql.DB
	Logger              *otelzap.Logger
	AESHashKey          string
	HTMLSanitizerPolicy *bluemonday.Policy
	Redis               *redis.Client
}

// CreateGame creates a new story pointing session
func (d *Service) CreateGame(ctx context.Context, facilitatorID string, name string, estimationScaleID string, pointValuesAllowed []string, stories []*thunderdome.Story, autoFinishVoting bool, pointAverageRounding string, joinCode string, facilitatorCode string, hideVoterIdentity bool) (*thunderdome.Poker, error) {
	var encryptedJoinCode string
	var encryptedLeaderCode string

	if joinCode != "" {
		EncryptedCode, codeErr := db.Encrypt(joinCode, d.AESHashKey)
		if codeErr != nil {
			return nil, fmt.Errorf("create poker encrypt join_code error: %v", codeErr)
		}
		encryptedJoinCode = EncryptedCode
	}

	if facilitatorCode != "" {
		EncryptedCode, codeErr := db.Encrypt(facilitatorCode, d.AESHashKey)
		if codeErr != nil {
			return nil, fmt.Errorf("create poker encrypt leader_code error: %v", codeErr)
		}
		encryptedLeaderCode = EncryptedCode
	}

	var b = &thunderdome.Poker{
		Name:                 name,
		Users:                make([]*thunderdome.PokerUser, 0),
		Stories:              make([]*thunderdome.Story, 0),
		VotingLocked:         true,
		PointValuesAllowed:   pointValuesAllowed,
		AutoFinishVoting:     autoFinishVoting,
		PointAverageRounding: pointAverageRounding,
		HideVoterIdentity:    hideVoterIdentity,
		Facilitators:         make([]string, 0),
		JoinCode:             joinCode,
		FacilitatorCode:      facilitatorCode,
		EstimationScaleID:    estimationScaleID,
	}
	b.Facilitators = append(b.Facilitators, facilitatorID)

	tx, err := d.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		d.Logger.Error("create poker error", zap.Error(err))
	}

	// Insert into poker table
	err = tx.QueryRow(
		`INSERT INTO thunderdome.poker (
			name, voting_locked, point_values_allowed, auto_finish_voting,
			point_average_rounding, hide_voter_identity, join_code, leader_code,
			estimation_scale_id, created_date, updated_date
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
		RETURNING id`,
		name, true, pointValuesAllowed, autoFinishVoting,
		pointAverageRounding, hideVoterIdentity, encryptedJoinCode, encryptedLeaderCode,
		estimationScaleID,
	).Scan(&b.ID)
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("create poker query error: %v", err)
	}

	// Insert facilitator
	_, err = tx.Exec(
		`INSERT INTO thunderdome.poker_facilitator (poker_id, user_id) VALUES ($1, $2);`,
		b.ID, facilitatorID,
	)
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("create poker facilitator error: %v", err)
	}

	// Insert stories
	for i, story := range stories {
		// 使用循环索引作为位置值，确保唯一性
		position := i
		_, err = tx.Exec(
			`INSERT INTO thunderdome.poker_story (
				poker_id, name, type, reference_id, link, description,
				acceptance_criteria, priority, position
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);`,
			b.ID, story.Name, story.Type, story.ReferenceID, story.Link,
			story.Description, story.AcceptanceCriteria, story.Priority,
			position,
		)
		if err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("create poker story error: %v", err)
		}
	}

	err = tx.Commit()
	if err != nil {
		return nil, fmt.Errorf("create poker commit error: %v", err)
	}

	// 获取完整的游戏数据，包括所有故事
	completeGame, err := d.GetGameByID(b.ID, facilitatorID)
	if err != nil {
		d.Logger.Error("Failed to get complete game data for caching",
			zap.Error(err),
			zap.String("game_id", b.ID))
		return b, nil
	}

	// 设置缓存
	if d.Redis != nil {
		d.Logger.Info("Attempting to set game cache", zap.String("game_id", b.ID))
		if gameJSON, err := json.Marshal(completeGame); err == nil {
			cacheKey := fmt.Sprintf("game:%s", b.ID)
			d.Logger.Info("Setting game cache",
				zap.String("game_id", b.ID),
				zap.String("cache_key", cacheKey),
				zap.Int("data_size", len(gameJSON)))

			if err := d.Redis.Set(context.Background(), cacheKey, gameJSON, 24*time.Hour).Err(); err != nil {
				d.Logger.Error("Failed to set game cache",
					zap.Error(err),
					zap.String("game_id", b.ID),
					zap.String("cache_key", cacheKey))
			} else {
				// 验证缓存是否设置成功
				exists, err := d.Redis.Exists(context.Background(), cacheKey).Result()
				if err != nil {
					d.Logger.Error("Failed to verify cache existence",
						zap.Error(err),
						zap.String("game_id", b.ID))
				} else {
					d.Logger.Info("Game cache verification",
						zap.String("game_id", b.ID),
						zap.Int64("exists", exists))
				}
			}
		} else {
			d.Logger.Error("Failed to marshal game data",
				zap.Error(err),
				zap.String("game_id", b.ID))
		}
	} else {
		d.Logger.Warn("Redis client is nil, skipping cache", zap.String("game_id", b.ID))
	}

	return b, nil
}

// TeamCreateGame creates a new story pointing session associated to a team
func (d *Service) TeamCreateGame(ctx context.Context, teamID string, facilitatorID string, name string, estimationScaleID string, pointValuesAllowed []string, stories []*thunderdome.Story, autoFinishVoting bool, pointAverageRounding string, joinCode string, facilitatorCode string, hideVoterIdentity bool) (*thunderdome.Poker, error) {
	var encryptedJoinCode string
	var encryptedLeaderCode string

	if joinCode != "" {
		EncryptedCode, codeErr := db.Encrypt(joinCode, d.AESHashKey)
		if codeErr != nil {
			return nil, fmt.Errorf("team create poker encrypt join_code error: %v", codeErr)
		}
		encryptedJoinCode = EncryptedCode
	}

	if facilitatorCode != "" {
		EncryptedCode, codeErr := db.Encrypt(facilitatorCode, d.AESHashKey)
		if codeErr != nil {
			return nil, fmt.Errorf("team create poker encrypt leader_code error: %v", codeErr)
		}
		encryptedLeaderCode = EncryptedCode
	}

	var b = &thunderdome.Poker{
		Name:                 name,
		Users:                make([]*thunderdome.PokerUser, 0),
		Stories:              make([]*thunderdome.Story, 0),
		VotingLocked:         true,
		PointValuesAllowed:   pointValuesAllowed,
		AutoFinishVoting:     autoFinishVoting,
		PointAverageRounding: pointAverageRounding,
		HideVoterIdentity:    hideVoterIdentity,
		Facilitators:         make([]string, 0),
		JoinCode:             joinCode,
		FacilitatorCode:      facilitatorCode,
		EstimationScaleID:    estimationScaleID,
		TeamID:               teamID,
	}
	b.Facilitators = append(b.Facilitators, facilitatorID)

	tx, err := d.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		d.Logger.Error("create poker error", zap.Error(err))
	}

	// Insert into poker table
	err = tx.QueryRow(
		`INSERT INTO thunderdome.poker (
			name, voting_locked, point_values_allowed, auto_finish_voting,
			point_average_rounding, hide_voter_identity, join_code, leader_code,
			estimation_scale_id, team_id, created_date, updated_date
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW())
		RETURNING id`,
		name, true, pointValuesAllowed, autoFinishVoting,
		pointAverageRounding, hideVoterIdentity, encryptedJoinCode, encryptedLeaderCode,
		estimationScaleID, teamID,
	).Scan(&b.ID)
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("create poker query error: %v", err)
	}

	// Insert facilitator
	_, err = tx.Exec(
		`INSERT INTO thunderdome.poker_facilitator (poker_id, user_id) VALUES ($1, $2);`,
		b.ID, facilitatorID,
	)
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("create poker facilitator error: %v", err)
	}

	// Insert stories
	for i, story := range stories {
		// 使用循环索引作为位置值，确保唯一性
		position := i
		_, err = tx.Exec(
			`INSERT INTO thunderdome.poker_story (
				poker_id, name, type, reference_id, link, description,
				acceptance_criteria, priority, position
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);`,
			b.ID, story.Name, story.Type, story.ReferenceID, story.Link,
			story.Description, story.AcceptanceCriteria, story.Priority,
			position,
		)
		if err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("create poker story error: %v", err)
		}
	}

	err = tx.Commit()
	if err != nil {
		return nil, fmt.Errorf("create poker commit error: %v", err)
	}

	// 获取完整的游戏数据，包括所有故事
	completeGame, err := d.GetGameByID(b.ID, facilitatorID)
	if err != nil {
		d.Logger.Error("Failed to get complete game data for caching",
			zap.Error(err),
			zap.String("game_id", b.ID))
		return b, nil
	}

	// 设置缓存
	if d.Redis != nil {
		if gameJSON, err := json.Marshal(completeGame); err == nil {
			cacheKey := fmt.Sprintf("game:%s", b.ID)
			if err := d.Redis.Set(context.Background(), cacheKey, gameJSON, 24*time.Hour).Err(); err != nil {
				d.Logger.Error("Failed to set game cache", zap.Error(err), zap.String("game_id", b.ID))
			} else {
				d.Logger.Info("Game cache set successfully", zap.String("game_id", b.ID))
			}
		}
	}

	return b, nil
}

// UpdateGame updates a game by ID
func (d *Service) UpdateGame(pokerID string, name string, pointValuesAllowed []string, autoFinishVoting bool, pointAverageRounding string, hideVoterIdentity bool, joinCode string, facilitatorCode string, teamID string) error {
	var encryptedJoinCode string
	var encryptedLeaderCode string

	if joinCode != "" {
		EncryptedCode, codeErr := db.Encrypt(joinCode, d.AESHashKey)
		if codeErr != nil {
			return fmt.Errorf("update poker encrypt join_code error: %v", codeErr)
		}
		encryptedJoinCode = EncryptedCode
	}

	if facilitatorCode != "" {
		EncryptedCode, codeErr := db.Encrypt(facilitatorCode, d.AESHashKey)
		if codeErr != nil {
			return fmt.Errorf("update poker encrypt leader_code error: %v", codeErr)
		}
		encryptedLeaderCode = EncryptedCode
	}

	if _, err := d.DB.Exec(`
		UPDATE thunderdome.poker
		SET name = $2, point_values_allowed = $3, auto_finish_voting = $4, point_average_rounding = $5,
		 hide_voter_identity = $6, join_code = $7, leader_code = $8, updated_date = NOW(), team_id = NULLIF($9, '')::uuid
		WHERE id = $1`,
		pokerID, name, pointValuesAllowed, autoFinishVoting, pointAverageRounding,
		hideVoterIdentity, encryptedJoinCode, encryptedLeaderCode, teamID,
	); err != nil {
		return fmt.Errorf("update poker query error: %v", err)
	}

	// 清除缓存
	if d.Redis != nil {
		cacheKey := fmt.Sprintf("game:%s", pokerID)
		d.Redis.Del(context.Background(), cacheKey)
	}

	return nil
}

// GetGameByID gets a game by ID
func (d *Service) GetGameByID(pokerID string, userID string) (*thunderdome.Poker, error) {
	// 尝试从Redis缓存获取
	cacheKey := fmt.Sprintf("game:%s", pokerID)
	if d.Redis != nil {
		if cachedData, err := d.Redis.Get(context.Background(), cacheKey).Result(); err == nil {
			var game thunderdome.Poker
			if err := json.Unmarshal([]byte(cachedData), &game); err == nil {
				d.Logger.Debug("Game cache hit", zap.String("game_id", pokerID))
				// 确保缓存中的游戏数据包含所有必要的信息
				if len(game.Stories) > 0 && len(game.Users) > 0 {
					return &game, nil
				} else {
					d.Logger.Warn("Incomplete game data in cache, fetching from database",
						zap.String("game_id", pokerID),
						zap.Int("stories_count", len(game.Stories)),
						zap.Int("users_count", len(game.Users)))
				}
			}
		}
	}

	var b = &thunderdome.Poker{
		ID:           pokerID,
		Users:        make([]*thunderdome.PokerUser, 0),
		Stories:      make([]*thunderdome.Story, 0),
		VotingLocked: true,
		Facilitators: make([]string, 0),
	}

	// get game
	var facilitators string
	var joinCode string
	var facilitatorCode string
	var estimationScaleJSON []byte
	var vArray pgtype.Array[string]
	m := pgtype.NewMap()
	e := d.DB.QueryRow(
		`
		SELECT b.id, b.name, b.voting_locked, COALESCE(b.active_story_id::text, ''), b.auto_finish_voting,
		b.point_average_rounding, b.hide_voter_identity, COALESCE(b.join_code, ''), COALESCE(b.leader_code, ''),
		b.estimation_scale_id, b.point_values_allowed, COALESCE(b.team_id::text, ''), b.created_date, b.updated_date,
		CASE WHEN COUNT(bl) = 0 THEN '[]'::json ELSE array_to_json(array_agg(bl.user_id)) END AS leaders,
		COALESCE(
			json_build_object(
				'id', es.id,
				'name', es.name,
				'description', es.description,
				'scale_type', es.scale_type,
				'values', es.values,
				'created_by', es.created_by,
				'created_at', es.created_at,
				'updated_at', es.updated_at,
				'is_public', es.is_public,
				'organization_id', es.organization_id,
				'team_id', es.team_id,
				'default_scale', es.default_scale
			)::jsonb,
			'{}'::jsonb
		) AS estimation_scale
		FROM thunderdome.poker b
		LEFT JOIN thunderdome.poker_facilitator bl ON b.id = bl.poker_id
		LEFT JOIN thunderdome.estimation_scale es ON b.estimation_scale_id = es.id
		WHERE b.id = $1
		GROUP BY b.id, es.id`,
		pokerID,
	).Scan(
		&b.ID,
		&b.Name,
		&b.VotingLocked,
		&b.ActiveStoryID,
		&b.AutoFinishVoting,
		&b.PointAverageRounding,
		&b.HideVoterIdentity,
		&joinCode,
		&facilitatorCode,
		&b.EstimationScaleID,
		m.SQLScanner(&vArray),
		&b.TeamID,
		&b.CreatedDate,
		&b.UpdatedDate,
		&facilitators,
		&estimationScaleJSON,
	)
	if e != nil {
		return nil, fmt.Errorf("get poker query error: %v", e)
	}

	b.PointValuesAllowed = vArray.Elements

	_ = json.Unmarshal([]byte(facilitators), &b.Facilitators)

	// Unmarshal the estimation scale JSON into the EstimationScale field
	if len(estimationScaleJSON) > 0 {
		var estimationScale thunderdome.EstimationScale
		if err := json.Unmarshal(estimationScaleJSON, &estimationScale); err != nil {
			return nil, fmt.Errorf("error unmarshaling estimation scale: %v", err)
		}
		b.EstimationScale = &estimationScale
	}

	isFacilitator := db.Contains(b.Facilitators, userID)

	if joinCode != "" {
		decryptedCode, codeErr := db.Decrypt(joinCode, d.AESHashKey)
		if codeErr != nil {
			return nil, fmt.Errorf("get poker decode join_code error: %v", codeErr)
		}
		b.JoinCode = decryptedCode
	}

	if facilitatorCode != "" && isFacilitator {
		decryptedCode, codeErr := db.Decrypt(facilitatorCode, d.AESHashKey)
		if codeErr != nil {
			return nil, fmt.Errorf("get poker decode leader_code error: %v", codeErr)
		}
		b.FacilitatorCode = decryptedCode
	}

	b.Users = d.GetUsers(pokerID)
	b.Stories = d.GetStories(pokerID, userID)

	// 设置缓存
	if d.Redis != nil {
		if gameJSON, err := json.Marshal(b); err == nil {
			d.Redis.Set(context.Background(), cacheKey, gameJSON, 24*time.Hour)
		}
	}

	return b, nil
}

// GetGamesByUser gets a list of games by UserID
func (d *Service) GetGamesByUser(userID string, limit int, offset int) ([]*thunderdome.Poker, int, error) {
	var count int
	var games = make([]*thunderdome.Poker, 0)

	// 注意：这里不使用Redis缓存，因为用户的游戏列表需要实时从数据库获取
	// 特别是在测试环境中，这确保了测试能够正确验证API功能

	e := d.DB.QueryRow(`
		WITH user_teams AS (
			SELECT t.id FROM thunderdome.team_user tu
			LEFT JOIN thunderdome.team t ON t.id = tu.team_id
			WHERE tu.user_id = $1
		),
		team_games AS (
			SELECT id FROM thunderdome.poker WHERE team_id IN (SELECT id FROM user_teams)
		),
		user_games AS (
			SELECT u.poker_id AS id FROM thunderdome.poker_user u
			WHERE u.user_id = $1 AND u.abandoned = false
		),
		games AS (
			SELECT id from user_games UNION SELECT id FROM team_games
		)
		SELECT COUNT(*) FROM games;
	`, userID).Scan(
		&count,
	)
	if e != nil {
		return nil, count, fmt.Errorf("get poker by user count query error: %v", e)
	}

	gameRows, gamesErr := d.DB.Query(`
		WITH user_teams AS (
			SELECT t.id, t.name FROM thunderdome.team_user tu
			LEFT JOIN thunderdome.team t ON t.id = tu.team_id
			WHERE tu.user_id = $1
		),
		team_games AS (
			SELECT id FROM thunderdome.poker WHERE team_id IN (SELECT id FROM user_teams)
		),
		user_games AS (
			SELECT u.poker_id AS id FROM thunderdome.poker_user u
			WHERE u.user_id = $1 AND u.abandoned = false
		),
		games AS (
			SELECT id from user_games UNION SELECT id FROM team_games
		),
		stories AS (
			SELECT poker_id, points FROM thunderdome.poker_story WHERE poker_id IN (SELECT poker_id FROM games)
		),
		facilitators AS (
			SELECT poker_id, user_id FROM thunderdome.poker_facilitator WHERE poker_id IN (SELECT poker_id FROM games)
		)
		SELECT p.id, p.name, p.voting_locked, COALESCE(p.active_story_id::text, ''), p.point_values_allowed, p.auto_finish_voting,
		  p.point_average_rounding, p.created_date, p.updated_date,
		  (SELECT CASE WHEN COUNT(s) = 0 THEN '[]'::json ELSE array_to_json(array_agg(row_to_json(s))) END
		  FROM thunderdome.poker_story s WHERE p.id = s.poker_id) AS stories,
		  (SELECT CASE WHEN COUNT(bl) = 0 THEN '[]'::json ELSE array_to_json(array_agg(bl.user_id)) END FROM
		  thunderdome.poker_facilitator bl WHERE bl.poker_id = p.id) AS facilitators,
		  min(COALESCE(t.name, '')) as team_name, COALESCE(p.team_id::TEXT, ''), p.estimation_scale_id,
		  COALESCE(
			json_build_object(
				'id', es.id,
				'name', es.name,
				'description', es.description,
				'scale_type', es.scale_type,
				'values', es.values,
				'created_by', es.created_by,
				'created_at', es.created_at,
				'updated_at', es.updated_at,
				'is_public', es.is_public,
				'organization_id', es.organization_id,
				'team_id', es.team_id,
				'default_scale', es.default_scale
			)::jsonb,
			'{}'::jsonb
		) AS estimation_scale
		FROM thunderdome.poker p
		LEFT JOIN user_teams t ON t.id = p.team_id
		LEFT JOIN thunderdome.estimation_scale es ON p.estimation_scale_id = es.id
		WHERE p.id IN (SELECT id FROM games)
		GROUP BY p.id, p.created_date, es.id
		ORDER BY p.created_date DESC
		LIMIT $2 OFFSET $3
	`, userID, limit, offset)
	if gamesErr != nil {
		d.Logger.Error("get poker by user query error", zap.Error(gamesErr))
		return nil, count, fmt.Errorf("get poker by user query error: %v", gamesErr)
	}

	defer gameRows.Close()
	for gameRows.Next() {
		var stories string
		var estimationScale string
		var vArray pgtype.Array[string]
		m := pgtype.NewMap()
		var facilitators string
		var b = &thunderdome.Poker{
			Users:              make([]*thunderdome.PokerUser, 0),
			Stories:            make([]*thunderdome.Story, 0),
			VotingLocked:       true,
			PointValuesAllowed: make([]string, 0),
			AutoFinishVoting:   true,
			Facilitators:       make([]string, 0),
		}
		if err := gameRows.Scan(
			&b.ID,
			&b.Name,
			&b.VotingLocked,
			&b.ActiveStoryID,
			m.SQLScanner(&vArray),
			&b.AutoFinishVoting,
			&b.PointAverageRounding,
			&b.CreatedDate,
			&b.UpdatedDate,
			&stories,
			&facilitators,
			&b.TeamName,
			&b.TeamID,
			&b.EstimationScaleID,
			&estimationScale,
		); err != nil {
			d.Logger.Error("error getting poker by user", zap.Error(err))
		} else {
			_ = json.Unmarshal([]byte(stories), &b.Stories)
			_ = json.Unmarshal([]byte(facilitators), &b.Facilitators)
			_ = json.Unmarshal([]byte(estimationScale), &b.EstimationScale)
			b.PointValuesAllowed = vArray.Elements

			games = append(games, b)
		}
	}

	return games, count, nil
}

// DeleteGame removes all game associations and the game itself by PokerID
func (d *Service) DeleteGame(pokerID string) error {
	if _, err := d.DB.Exec(
		`DELETE FROM thunderdome.poker WHERE id = $1;`, pokerID); err != nil {
		return fmt.Errorf("poker delete query error: %v", err)
	}

	// 清除缓存
	if d.Redis != nil {
		cacheKey := fmt.Sprintf("game:%s", pokerID)
		d.Redis.Del(context.Background(), cacheKey)
	}

	return nil
}

// GetGames gets a list of games
func (d *Service) GetGames(limit int, offset int) ([]*thunderdome.Poker, int, error) {
	var games = make([]*thunderdome.Poker, 0)
	var count int

	e := d.DB.QueryRow(
		"SELECT COUNT(*) FROM thunderdome.poker;",
	).Scan(
		&count,
	)
	if e != nil {
		return nil, count, fmt.Errorf("get poker games count query error: %v", e)
	}

	rows, gamesErr := d.DB.Query(`
		SELECT b.id, b.name, b.voting_locked, b.active_story_id, b.point_values_allowed,
		 b.auto_finish_voting, b.point_average_rounding, b.created_date, b.updated_date, COALESCE(b.team_id::TEXT, ''),
		CASE WHEN COUNT(bl) = 0 THEN '[]'::json ELSE array_to_json(array_agg(bl.user_id)) END AS leaders
		FROM thunderdome.poker b
		LEFT JOIN thunderdome.poker_facilitator bl ON b.id = bl.poker_id
		GROUP BY b.id, b.created_date ORDER BY b.created_date DESC
		LIMIT $1 OFFSET $2;
	`, limit, offset)
	if gamesErr != nil {
		return nil, count, fmt.Errorf("get poker games query error: %v", gamesErr)
	}

	defer rows.Close()
	for rows.Next() {
		var vArray pgtype.Array[string]
		m := pgtype.NewMap()
		var facilitators string
		var activeStoryID sql.NullString
		var b = &thunderdome.Poker{
			Users:              make([]*thunderdome.PokerUser, 0),
			Stories:            make([]*thunderdome.Story, 0),
			VotingLocked:       true,
			PointValuesAllowed: make([]string, 0),
			AutoFinishVoting:   true,
			Facilitators:       make([]string, 0),
		}
		if err := rows.Scan(
			&b.ID,
			&b.Name,
			&b.VotingLocked,
			&activeStoryID,
			m.SQLScanner(&vArray),
			&b.AutoFinishVoting,
			&b.PointAverageRounding,
			&b.CreatedDate,
			&b.UpdatedDate,
			&b.TeamID,
			&facilitators,
		); err != nil {
			d.Logger.Error("get poker games query error", zap.Error(err))
		} else {
			b.PointValuesAllowed = vArray.Elements
			_ = json.Unmarshal([]byte(facilitators), &b.Facilitators)
			b.ActiveStoryID = activeStoryID.String
			games = append(games, b)
		}
	}

	return games, count, nil
}

// GetActiveGames gets a list of active games
func (d *Service) GetActiveGames(limit int, offset int) ([]*thunderdome.Poker, int, error) {
	var games = make([]*thunderdome.Poker, 0)
	var count int

	e := d.DB.QueryRow(
		"SELECT COUNT(DISTINCT pu.poker_id) FROM thunderdome.poker_user pu WHERE pu.active IS TRUE;",
	).Scan(
		&count,
	)
	if e != nil {
		return nil, count, fmt.Errorf("get active poker games count query error: %v", e)
	}

	rows, gamesErr := d.DB.Query(`
		SELECT b.id, b.name, b.voting_locked, b.active_story_id, b.point_values_allowed, b.auto_finish_voting,
		 b.point_average_rounding, b.created_date, b.updated_date, COALESCE(b.team_id::TEXT, ''),
		CASE WHEN COUNT(bl) = 0 THEN '[]'::json ELSE array_to_json(array_agg(bl.user_id)) END AS leaders
		FROM thunderdome.poker_user bu
		LEFT JOIN thunderdome.poker b ON b.id = bu.poker_id
		LEFT JOIN thunderdome.poker_facilitator bl ON b.id = bl.poker_id
		WHERE bu.active IS TRUE GROUP BY b.id
		LIMIT $1 OFFSET $2;
	`, limit, offset)
	if gamesErr != nil {
		return nil, count, fmt.Errorf("get active poker games query error: %v", gamesErr)
	}

	defer rows.Close()
	for rows.Next() {
		var vArray pgtype.Array[string]
		m := pgtype.NewMap()
		var facilitators string
		var activeStoryID sql.NullString
		var b = &thunderdome.Poker{
			Users:              make([]*thunderdome.PokerUser, 0),
			Stories:            make([]*thunderdome.Story, 0),
			VotingLocked:       true,
			PointValuesAllowed: make([]string, 0),
			AutoFinishVoting:   true,
			Facilitators:       make([]string, 0),
		}
		if err := rows.Scan(
			&b.ID,
			&b.Name,
			&b.VotingLocked,
			&activeStoryID,
			m.SQLScanner(&vArray),
			&b.AutoFinishVoting,
			&b.PointAverageRounding,
			&b.CreatedDate,
			&b.UpdatedDate,
			&b.TeamID,
			&facilitators,
		); err != nil {
			d.Logger.Error("get active poker games query error", zap.Error(err))
		} else {
			b.PointValuesAllowed = vArray.Elements
			_ = json.Unmarshal([]byte(facilitators), &b.Facilitators)
			b.ActiveStoryID = activeStoryID.String
			games = append(games, b)
		}
	}

	return games, count, nil
}
