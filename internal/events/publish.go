package events

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

type PublishLayout interface {
	// ReadyToPublish checks the current layout and saleable inventory for every
	// supplied slot, including in a read-only transaction. Layout writes must
	// acquire the same event row lock as publication.
	ReadyToPublish(ctx context.Context, tx pgx.Tx, eventID string, slotIDs []string) (bool, error)
}

type publicationReadiness struct {
	Ready  bool        `json:"ready"`
	Fields fieldErrors `json:"fields"`
}

func (s Store) PublishRoutes(api *gin.RouterGroup, sessions *auth.Service, origin string, layout PublishLayout) error {
	if s.DB == nil || sessions == nil || origin == "" || layout == nil {
		return errors.New("publish requires a configured event layout source")
	}
	api.GET("/events/:id/publication-readiness", sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		id := c.Param("id")
		if !validResource(c, id) {
			return
		}
		result, err := s.publicationReadiness(c.Request.Context(), account.ID, id, layout)
		if err != nil {
			writeError(c, err)
			return
		}
		httpapi.Success(c, http.StatusOK, result)
	}))
	api.POST("/events/:id/publish", httpapi.RequireOrigin(origin), sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		id := c.Param("id")
		if !validResource(c, id) {
			return
		}
		data, err := s.publishDraft(c.Request.Context(), account.ID, id, layout)
		var fields fieldErrors
		if errors.As(err, &fields) {
			httpapi.Error(c, http.StatusUnprocessableEntity, "EVENT_NOT_READY", "Lengkapi syarat publikasi acara.", fields)
			return
		}
		writeEvent(c, http.StatusOK, data, err)
	}))
	return nil
}

func (s Store) publicationReadiness(ctx context.Context, actor, id string, layout PublishLayout) (publicationReadiness, error) {
	result := publicationReadiness{Fields: make(fieldErrors)}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		current, err := scanEvent(tx.QueryRow(ctx, `SELECT `+eventColumns+` FROM events e
			JOIN memberships m ON m.organization_id = e.organization_id
			WHERE e.id = $1 AND m.user_id = $2 AND m.role = 'owner'`, id, actor))
		if err != nil {
			return err
		}
		if current.Status != "draft" {
			return errDraftRequired
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
			return err
		}
		err = s.withPlan(ctx, tx, current.ID, checkPublication(ctx, tx, current, layout, now))
		var fields fieldErrors
		if errors.As(err, &fields) {
			result.Fields = fields
			return nil
		}
		result.Ready = err == nil
		return err
	})
	return result, err
}

func (s Store) publishDraft(ctx context.Context, actor, id string, layout PublishLayout) (event, error) {
	var result event
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		current, err := lockOwnedEvent(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		if current.Status == "published" {
			result = current
			return nil
		}
		if current.Status != "draft" {
			return errDraftRequired
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		if err := s.withPlan(ctx, tx, current.ID, checkPublication(ctx, tx, current, layout, now)); err != nil {
			return err
		}
		result, err = scanEvent(tx.QueryRow(ctx, `UPDATE events e SET status = 'published', updated_at = clock_timestamp()
			WHERE e.id = $1 RETURNING `+eventColumns, id))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO event_status_history (event_id, actor_id, from_status, to_status)
			VALUES ($1, $2, 'draft', 'published')`, id, actor)
		return err
	})
	return result, err
}

func checkPublication(ctx context.Context, tx pgx.Tx, current event, layout PublishLayout, now time.Time) error {
	fields := make(fieldErrors)
	if current.Terms == nil || strings.TrimSpace(*current.Terms) == "" {
		fields["terms"] = "Isi syarat acara sebelum terbit."
	}
	if !current.EndsAt.After(now) {
		fields["ends_at"] = "Acara harus belum berakhir saat diterbitkan."
	}
	slots, err := pricingSlots(ctx, tx, current.ID)
	if err != nil {
		return err
	}
	var slotIDs []string
	for _, slot := range slots {
		if slot.StartsAt.Before(current.StartsAt) || slot.EndsAt.After(current.EndsAt) {
			fields["slots"] = "Jadwal slot harus berada dalam rentang acara."
		}
		if slot.EndsAt.After(now) {
			slotIDs = append(slotIDs, slot.ID)
		}
	}
	if len(slotIDs) == 0 {
		fields["slots"] = "Sediakan minimal satu slot yang belum berakhir."
	} else {
		ready, err := layout.ReadyToPublish(ctx, tx, current.ID, slotIDs)
		if err != nil {
			return err
		}
		if !ready {
			fields["layout"] = "Lengkapi denah dan sediakan stand aktif untuk setiap slot."
		}
		if err := checkPublishedPrices(ctx, tx, fields, current, slots, now); err != nil {
			return err
		}
	}
	if len(fields) > 0 {
		return fields
	}
	return nil
}

func checkPublishedPrices(ctx context.Context, tx pgx.Tx, fields fieldErrors, current event, slots []pricedSlot, now time.Time) error {
	active, err := loadPriceVersion(ctx, tx, activePricingQuery, current.ID, now)
	if err != nil {
		return err
	}
	if active == nil || !coversOpenSlots(active.Rules, slots, now) {
		fields["pricing"] = "Tetapkan harga aktif untuk setiap slot yang belum berakhir."
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (effective_at) revision, effective_at, rules
		FROM event_pricing_versions WHERE event_id = $1 AND effective_at > $2 AND effective_at < $3
		ORDER BY effective_at, revision DESC`, current.ID, now, current.EndsAt)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var revision int
		var effective time.Time
		var rules priceRules
		if err := rows.Scan(&revision, &effective, &rules); err != nil {
			return err
		}
		if !coversOpenSlots(rules, slots, effective) {
			fields["pricing"] = fmt.Sprintf("Versi harga terjadwal %d belum mencakup slot yang masih tersedia saat versi mulai berlaku.", revision)
			break
		}
	}
	return rows.Err()
}

func coversOpenSlots(rules priceRules, slots []pricedSlot, at time.Time) bool {
	for _, slot := range slots {
		if slot.EndsAt.After(at) && rules.baseForSlot(slot.ID) == nil {
			return false
		}
	}
	return true
}

// withPlan adds the paid-plan requirement to the result of checkPublication when it is enabled.
func (s Store) withPlan(ctx context.Context, tx pgx.Tx, eventID string, err error) error {
	if !s.RequirePlan {
		return err
	}
	var fields fieldErrors
	if err != nil && !errors.As(err, &fields) {
		return err
	}
	if fields == nil {
		fields = make(fieldErrors)
	}
	var stands int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM stands
		WHERE event_id = $1 AND removed_at IS NULL AND active`, eventID).Scan(&stands); err != nil {
		return err
	}
	issue, err := planIssue(ctx, tx, eventID, stands)
	if err != nil {
		return err
	}
	if issue != "" {
		fields["plan"] = issue
	}
	if len(fields) > 0 {
		return fields
	}
	return nil
}

func planIssue(ctx context.Context, tx pgx.Tx, eventID string, stands int) (string, error) {
	var capacity *int
	err := tx.QueryRow(ctx, `SELECT capacity_stands FROM event_plan_purchases
		WHERE event_id = $1 AND status = 'active'`, eventID).Scan(&capacity)
	if errors.Is(err, pgx.ErrNoRows) {
		return "Aktifkan paket acara untuk melanjutkan.", nil
	}
	if err != nil {
		return "", err
	}
	if capacity != nil && stands > *capacity {
		return fmt.Sprintf("Paket mendukung maksimal %d stand aktif, denah memiliki %d.", *capacity, stands), nil
	}
	return "", nil
}
