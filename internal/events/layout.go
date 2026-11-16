package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const (
	minGridCells   = 4
	maxGridCells   = 60
	minCellMeters  = 0.5
	maxCellMeters  = 10
	maxLayoutItems = 500
)

var (
	errLayoutConflict     = errors.New("layout revision is stale")
	errLayoutPrecondition = errors.New("layout revision header is required")
	errLayoutClosed       = errors.New("layout can no longer change")
)

// reservedStandError names the stand that has an active hold, offer, or reservation.
type reservedStandError struct{ code string }

func (err reservedStandError) Error() string {
	return "reserved stand cannot change silently: " + err.code
}

var areaKinds = map[string]string{
	"walkway": "lorong", "entrance": "pintu masuk", "stage": "panggung", "restricted": "area terlarang",
}

type layoutRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type layoutBlock struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	layoutRect
}

type layoutArea struct {
	Kind string `json:"kind"`
	layoutRect
}

type layoutStand struct {
	ID          string   `json:"stand_id"`
	BlockID     *string  `json:"block_id,omitempty"`
	Code        string   `json:"code"`
	Rotation    int      `json:"rotation"`
	CategoryIDs []string `json:"category_ids"`
	Facilities  []string `json:"facilities"`
	Active      bool     `json:"active"`
	layoutRect
}

// span is the footprint on the grid after rotation, matching the frontend editor.
func (stand layoutStand) span() layoutRect {
	if stand.Rotation == 90 {
		return layoutRect{X: stand.X, Y: stand.Y, Width: stand.Height, Height: stand.Width}
	}
	return stand.layoutRect
}

type layoutInput struct {
	Width      int           `json:"width"`
	Height     int           `json:"height"`
	CellMeters float64       `json:"cell_meters"`
	Blocks     []layoutBlock `json:"blocks"`
	Stands     []layoutStand `json:"stands"`
	Areas      []layoutArea  `json:"areas"`
}

type layoutRevision struct {
	EventID  string `json:"event_id"`
	Revision int    `json:"revision"`
	layoutInput
}

// publicStand adds what visitors need: availability and the price from the active pricing rules.
type publicStand struct {
	layoutStand
	Status    string `json:"status"`
	BasePrice *int64 `json:"base_price,omitempty"`
}

type publicLayout struct {
	EventID    string        `json:"event_id"`
	Revision   int           `json:"revision"`
	Width      int           `json:"width"`
	Height     int           `json:"height"`
	CellMeters float64       `json:"cell_meters"`
	Blocks     []layoutBlock `json:"blocks"`
	Areas      []layoutArea  `json:"areas"`
	Stands     []publicStand `json:"stands"`
}

func overlaps(a, b layoutRect) bool {
	return a.X < b.X+b.Width && b.X < a.X+a.Width && a.Y < b.Y+b.Height && b.Y < a.Y+a.Height
}

func (input layoutInput) inBounds(rect layoutRect) bool {
	return rect.X >= 0 && rect.Y >= 0 && rect.Width >= 1 && rect.Height >= 1 &&
		rect.X+rect.Width <= input.Width && rect.Y+rect.Height <= input.Height
}

// validate checks geometry and references. It mirrors the editor so the server rejects what the editor flags.
func (input *layoutInput) validate() fieldErrors {
	fields := make(fieldErrors)
	if input.Width < minGridCells || input.Width > maxGridCells || input.Height < minGridCells || input.Height > maxGridCells {
		fields["width"] = fmt.Sprintf("Ukuran grid harus %d sampai %d petak.", minGridCells, maxGridCells)
	}
	if input.CellMeters < minCellMeters || input.CellMeters > maxCellMeters {
		fields["cell_meters"] = "Ukuran petak harus 0,5 sampai 10 meter."
	}
	if len(input.Blocks)+len(input.Stands)+len(input.Areas) > maxLayoutItems {
		fields["stands"] = fmt.Sprintf("Denah maksimal berisi %d objek.", maxLayoutItems)
		return fields
	}
	if input.Blocks == nil {
		input.Blocks = []layoutBlock{}
	}
	if input.Stands == nil {
		input.Stands = []layoutStand{}
	}
	if input.Areas == nil {
		input.Areas = []layoutArea{}
	}

	blocks := make(map[string]bool, len(input.Blocks))
	for i := range input.Blocks {
		block := &input.Blocks[i]
		block.ID = strings.ToLower(block.ID)
		block.Label = strings.TrimSpace(block.Label)
		switch {
		case !httpapi.ValidID(block.ID) || blocks[block.ID]:
			fields["blocks"] = "Setiap blok perlu ID unik."
		case !httpapi.ValidSingleLine(block.Label, 40):
			fields["blocks"] = "Isi nama blok hingga 40 karakter."
		case !input.inBounds(block.layoutRect):
			fields["blocks"] = fmt.Sprintf("Blok %s keluar dari batas grid.", block.Label)
		}
		blocks[block.ID] = true
		for _, other := range input.Blocks[:i] {
			if overlaps(block.layoutRect, other.layoutRect) {
				fields["blocks"] = fmt.Sprintf("Blok %s bertumpuk dengan blok %s.", block.Label, other.Label)
			}
		}
	}

	for _, area := range input.Areas {
		if _, ok := areaKinds[area.Kind]; !ok {
			fields["areas"] = "Pilih jenis area yang valid."
		} else if !input.inBounds(area.layoutRect) {
			fields["areas"] = fmt.Sprintf("Area %s keluar dari batas grid.", areaKinds[area.Kind])
		}
	}

	ids := make(map[string]bool, len(input.Stands))
	codes := make(map[string]bool, len(input.Stands))
	for i := range input.Stands {
		stand := &input.Stands[i]
		stand.ID = strings.ToLower(stand.ID)
		stand.Code = strings.ToUpper(strings.TrimSpace(stand.Code))
		name := stand.Code
		switch {
		case !httpapi.ValidID(stand.ID) || ids[stand.ID]:
			fields["stands"] = "Setiap stand perlu ID unik."
		case !httpapi.ValidSingleLine(stand.Code, 12):
			fields["stands"] = "Isi kode stand hingga 12 karakter."
		case codes[stand.Code]:
			fields["stands"] = fmt.Sprintf("Kode %s dipakai lebih dari satu stand.", name)
		case stand.Rotation != 0 && stand.Rotation != 90:
			fields["stands"] = fmt.Sprintf("Rotasi %s hanya boleh 0 atau 90 derajat.", name)
		case !input.inBounds(stand.span()):
			fields["stands"] = fmt.Sprintf("%s keluar dari batas grid.", name)
		}
		ids[stand.ID], codes[stand.Code] = true, true
		if stand.BlockID != nil {
			id := strings.ToLower(*stand.BlockID)
			stand.BlockID = &id
			if !blocks[id] {
				fields["stands"] = fmt.Sprintf("Blok untuk %s tidak ada di denah.", name)
			}
		}
		if stand.CategoryIDs == nil {
			stand.CategoryIDs = []string{}
		}
		for _, category := range stand.CategoryIDs {
			if !httpapi.ValidID(category) {
				fields["stands"] = fmt.Sprintf("Kategori %s tidak valid.", name)
			}
		}
		if stand.Facilities == nil {
			stand.Facilities = []string{}
		}
		if len(stand.Facilities) > 20 {
			fields["stands"] = fmt.Sprintf("Fasilitas %s maksimal 20.", name)
		} else {
			for j, facility := range stand.Facilities {
				stand.Facilities[j] = strings.TrimSpace(facility)
				if !httpapi.ValidSingleLine(stand.Facilities[j], 120) {
					fields["stands"] = fmt.Sprintf("Nama fasilitas %s tidak valid.", name)
				}
			}
		}
		for _, other := range input.Stands[:i] {
			if overlaps(stand.span(), other.span()) {
				fields["stands"] = fmt.Sprintf("%s tumpang tindih dengan %s.", name, other.Code)
			}
		}
		for _, area := range input.Areas {
			if overlaps(stand.span(), area.layoutRect) {
				fields["stands"] = fmt.Sprintf("%s menimpa %s.", name, areaKinds[area.Kind])
			}
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// layoutETag uses the revision as a strong validator, for example "3". Zero means no layout yet.
func layoutETag(revision int) string { return fmt.Sprintf(`"%d"`, revision) }

func currentLayoutRevision(ctx context.Context, tx pgx.Tx, eventID string) (int, error) {
	var revision int
	err := tx.QueryRow(ctx, `SELECT revision FROM event_layouts WHERE event_id = $1 FOR UPDATE`, eventID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

// saveLayout replaces the whole layout as one revision. ifMatch is the client's ETag, empty when absent.
func (s Store) saveLayout(ctx context.Context, actor, eventID, ifMatch string, input layoutInput) (layoutRevision, error) {
	var result layoutRevision
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		owned, err := lockOwnedEvent(ctx, tx, actor, eventID)
		if err != nil {
			return err
		}
		if owned.Status == "completed" || owned.Status == "cancelled" {
			return errLayoutClosed
		}
		current, err := currentLayoutRevision(ctx, tx, eventID)
		if err != nil {
			return err
		}
		switch {
		case ifMatch == "" && current > 0:
			return errLayoutPrecondition
		case ifMatch != "" && ifMatch != layoutETag(current):
			return errLayoutConflict
		}
		if err := checkCategories(ctx, tx, input.Stands); err != nil {
			return err
		}
		if err := protectReservedStands(ctx, tx, eventID, input.Stands); err != nil {
			return err
		}

		next := current + 1
		_, err = tx.Exec(ctx, `INSERT INTO event_layouts (event_id, revision, width, height, cell_meters, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (event_id) DO UPDATE SET revision = EXCLUDED.revision,
			width = EXCLUDED.width, height = EXCLUDED.height, cell_meters = EXCLUDED.cell_meters,
			updated_by = EXCLUDED.updated_by, updated_at = clock_timestamp()`,
			eventID, next, input.Width, input.Height, input.CellMeters, actor)
		if err != nil {
			return err
		}
		if err := replaceLayoutItems(ctx, tx, eventID, input); err != nil {
			return err
		}
		result = layoutRevision{EventID: eventID, Revision: next, layoutInput: input}
		_, err = tx.Exec(ctx, `INSERT INTO event_layout_revisions (event_id, revision, actor_id, snapshot)
			VALUES ($1, $2, $3, $4)`, eventID, next, actor, input)
		return err
	})
	return result, err
}

func checkCategories(ctx context.Context, tx pgx.Tx, stands []layoutStand) error {
	seen := make(map[string]bool)
	categories := make([]string, 0)
	for _, stand := range stands {
		for _, id := range stand.CategoryIDs {
			if !seen[id] {
				seen[id] = true
				categories = append(categories, id)
			}
		}
	}
	if len(categories) == 0 {
		return nil
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM business_categories WHERE id = ANY($1::text[]::uuid[])`,
		categories).Scan(&count); err != nil {
		return err
	}
	if count != len(categories) {
		return fieldErrors{"stands": "Pilih kategori usaha yang tersedia."}
	}
	return nil
}

// protectReservedStands refuses to remove, move, or resize a stand with an active hold, offer, or reservation.
func protectReservedStands(ctx context.Context, tx pgx.Tx, eventID string, stands []layoutStand) error {
	rows, err := tx.Query(ctx, `SELECT s.id::text, s.code, s.x, s.y, s.width, s.height, s.rotation FROM stands s
		WHERE s.event_id = $1 AND s.removed_at IS NULL AND EXISTS (SELECT 1 FROM allocations a
		WHERE a.event_id = s.event_id AND a.stand_id = s.id AND a.status IN ('hold', 'offer', 'reserved'))`, eventID)
	if err != nil {
		return err
	}
	type reserved struct {
		id, code string
		rect     layoutRect
		rotation int
	}
	locked, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (reserved, error) {
		var value reserved
		err := row.Scan(&value.id, &value.code, &value.rect.X, &value.rect.Y, &value.rect.Width, &value.rect.Height, &value.rotation)
		return value, err
	})
	if err != nil {
		return err
	}
	next := make(map[string]layoutStand, len(stands))
	for _, stand := range stands {
		next[stand.ID] = stand
	}
	for _, stand := range locked {
		updated, kept := next[stand.id]
		if !kept || updated.layoutRect != stand.rect || updated.Rotation != stand.rotation || !updated.Active {
			return reservedStandError{code: stand.code}
		}
	}
	return nil
}

func replaceLayoutItems(ctx context.Context, tx pgx.Tx, eventID string, input layoutInput) error {
	keepBlocks := make([]string, 0, len(input.Blocks))
	for _, block := range input.Blocks {
		keepBlocks = append(keepBlocks, block.ID)
		tag, err := tx.Exec(ctx, `INSERT INTO layout_blocks (id, event_id, label, x, y, width, height)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (id) DO UPDATE SET label = EXCLUDED.label,
			x = EXCLUDED.x, y = EXCLUDED.y, width = EXCLUDED.width, height = EXCLUDED.height
			WHERE layout_blocks.event_id = EXCLUDED.event_id`,
			block.ID, eventID, block.Label, block.X, block.Y, block.Width, block.Height)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fieldErrors{"blocks": "Setiap blok perlu ID unik."}
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM layout_blocks WHERE event_id = $1 AND id <> ALL($2::text[]::uuid[])`,
		eventID, keepBlocks); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM layout_areas WHERE event_id = $1`, eventID); err != nil {
		return err
	}
	for _, area := range input.Areas {
		_, err := tx.Exec(ctx, `INSERT INTO layout_areas (event_id, kind, x, y, width, height)
			VALUES ($1, $2, $3, $4, $5, $6)`, eventID, area.Kind, area.X, area.Y, area.Width, area.Height)
		if err != nil {
			return err
		}
	}

	// Free codes of removed stands first so a stand can take over a code in the same revision.
	keepStands := make([]string, 0, len(input.Stands))
	for _, stand := range input.Stands {
		keepStands = append(keepStands, stand.ID)
	}
	if _, err := tx.Exec(ctx, `UPDATE stands SET removed_at = clock_timestamp()
		WHERE event_id = $1 AND removed_at IS NULL AND id <> ALL($2::text[]::uuid[])`, eventID, keepStands); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE stands SET code = '~' || left(id::text, 11)
		WHERE event_id = $1 AND removed_at IS NULL`, eventID); err != nil {
		return err
	}
	for _, stand := range input.Stands {
		tag, err := tx.Exec(ctx, `INSERT INTO stands (id, event_id, block_id, code, x, y, width, height, rotation,
			category_ids, facilities, active) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text[]::uuid[], $11, $12)
			ON CONFLICT (id) DO UPDATE SET block_id = EXCLUDED.block_id, code = EXCLUDED.code, x = EXCLUDED.x,
			y = EXCLUDED.y, width = EXCLUDED.width, height = EXCLUDED.height, rotation = EXCLUDED.rotation,
			category_ids = EXCLUDED.category_ids, facilities = EXCLUDED.facilities, active = EXCLUDED.active,
			removed_at = NULL WHERE stands.event_id = EXCLUDED.event_id`,
			stand.ID, eventID, stand.BlockID, stand.Code, stand.X, stand.Y, stand.Width, stand.Height, stand.Rotation,
			stand.CategoryIDs, stand.Facilities, stand.Active)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// The ID belongs to a stand of another event.
			return fieldErrors{"stands": "Setiap stand perlu ID unik."}
		}
	}
	return nil
}

// loadLayout reads the stored layout. It returns pgx.ErrNoRows when the event has no layout yet.
func loadLayout(ctx context.Context, tx pgx.Tx, eventID string) (layoutRevision, error) {
	result := layoutRevision{EventID: eventID}
	err := tx.QueryRow(ctx, `SELECT revision, width, height, cell_meters::float8 FROM event_layouts WHERE event_id = $1`,
		eventID).Scan(&result.Revision, &result.Width, &result.Height, &result.CellMeters)
	if err != nil {
		return layoutRevision{}, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text, label, x, y, width, height FROM layout_blocks
		WHERE event_id = $1 ORDER BY y, x, id`, eventID)
	if err != nil {
		return layoutRevision{}, err
	}
	result.Blocks, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (layoutBlock, error) {
		var value layoutBlock
		err := row.Scan(&value.ID, &value.Label, &value.X, &value.Y, &value.Width, &value.Height)
		return value, err
	})
	if err != nil {
		return layoutRevision{}, err
	}
	rows, err = tx.Query(ctx, `SELECT kind, x, y, width, height FROM layout_areas WHERE event_id = $1 ORDER BY y, x, id`, eventID)
	if err != nil {
		return layoutRevision{}, err
	}
	result.Areas, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (layoutArea, error) {
		var value layoutArea
		err := row.Scan(&value.Kind, &value.X, &value.Y, &value.Width, &value.Height)
		return value, err
	})
	if err != nil {
		return layoutRevision{}, err
	}
	rows, err = tx.Query(ctx, `SELECT id::text, block_id::text, code, x, y, width, height, rotation,
		category_ids::text[], facilities, active FROM stands WHERE event_id = $1 AND removed_at IS NULL ORDER BY code, id`, eventID)
	if err != nil {
		return layoutRevision{}, err
	}
	result.Stands, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (layoutStand, error) {
		var value layoutStand
		err := row.Scan(&value.ID, &value.BlockID, &value.Code, &value.X, &value.Y, &value.Width, &value.Height,
			&value.Rotation, &value.CategoryIDs, &value.Facilities, &value.Active)
		return value, err
	})
	if err != nil {
		return layoutRevision{}, err
	}
	return result, nil
}

// publicLayoutFor adds availability and current prices. Only published and live events are visible.
func (s Store) publicLayoutFor(ctx context.Context, eventID string) (publicLayout, error) {
	var result publicLayout
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM events WHERE id = $1`, eventID).Scan(&status); err != nil {
			return err
		}
		if status != "published" && status != "live" {
			return pgx.ErrNoRows
		}
		layout, err := loadLayout(ctx, tx, eventID)
		if err != nil {
			return err
		}
		taken, err := takenStands(ctx, tx, eventID)
		if err != nil {
			return err
		}
		prices, err := standPrices(ctx, tx, eventID, layout, taken)
		if err != nil {
			return err
		}
		result = publicLayout{
			EventID: layout.EventID, Revision: layout.Revision, Width: layout.Width, Height: layout.Height,
			CellMeters: layout.CellMeters, Blocks: layout.Blocks, Areas: layout.Areas,
			Stands: make([]publicStand, 0, len(layout.Stands)),
		}
		for _, stand := range layout.Stands {
			value := publicStand{layoutStand: stand, Status: "available", BasePrice: prices[stand.ID]}
			switch {
			case taken[stand.ID] == "reserved":
				value.Status = "booked"
			case !stand.Active || taken[stand.ID] != "":
				value.Status = "unavailable"
			}
			result.Stands = append(result.Stands, value)
		}
		return nil
	})
	return result, err
}

// standPrices quotes each active stand with the active pricing rules for the whole-event slot.
func standPrices(ctx context.Context, tx pgx.Tx, eventID string, layout layoutRevision, taken map[string]string) (map[string]*int64, error) {
	prices := make(map[string]*int64, len(layout.Stands))
	active, err := loadPriceVersion(ctx, tx, activePricingQuery, eventID, time.Now())
	if err != nil || active == nil {
		return prices, err
	}
	var slotID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM event_slots WHERE event_id = $1 ORDER BY starts_at, id LIMIT 1`, eventID).Scan(&slotID)
	if err != nil {
		return prices, err
	}
	capacity, confirmed := int32(0), int32(0)
	for _, stand := range layout.Stands {
		if stand.Active {
			capacity++
			if taken[stand.ID] == "reserved" {
				confirmed++
			}
		}
	}
	labels := make(map[string]string, len(layout.Blocks))
	for _, block := range layout.Blocks {
		labels[block.ID] = block.Label
	}
	for _, stand := range layout.Stands {
		if !stand.Active {
			continue
		}
		target := HoldStand{ID: stand.ID, Code: stand.Code, BlockID: stand.BlockID, AllowedCategoryIDs: stand.CategoryIDs}
		if stand.BlockID != nil {
			target.BlockLabel = labels[*stand.BlockID]
		}
		quote, err := active.Rules.quote(priceTarget{slotID: slotID, stand: &target}, confirmed, capacity)
		if err != nil {
			continue
		}
		rent := quote.Rent
		prices[stand.ID] = &rent
	}
	return prices, nil
}

// LayoutInventory serves active stands from the event layout to pricing and holds.
// Every slot of an event shares the same layout.
type LayoutInventory struct{}

var _ HoldInventory = LayoutInventory{}

func (LayoutInventory) ActiveStands(ctx context.Context, tx pgx.Tx, eventID, _ string) ([]HoldStand, error) {
	rows, err := tx.Query(ctx, `SELECT s.id::text, s.code, s.block_id::text, COALESCE(b.label, ''), s.category_ids::text[]
		FROM stands s LEFT JOIN layout_blocks b ON b.id = s.block_id AND b.event_id = s.event_id
		WHERE s.event_id = $1 AND s.removed_at IS NULL AND s.active ORDER BY s.code, s.id`, eventID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (HoldStand, error) {
		var stand HoldStand
		err := row.Scan(&stand.ID, &stand.Code, &stand.BlockID, &stand.BlockLabel, &stand.AllowedCategoryIDs)
		return stand, err
	})
}
