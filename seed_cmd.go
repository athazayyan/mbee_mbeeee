package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
	"kitakarsa.com/tenantku/backend/internal/events"
	"kitakarsa.com/tenantku/backend/internal/platform/adminperm"
)

//go:embed seed_assets/*.jpg seed_assets/*.webp
var seedPhotos embed.FS

// seedDemo fills a development database with an admin, five organizers with one published event each, and tenants
// with products. It is safe to run again: accounts keep their ids (and get their password reset), events are skipped
// when their slug exists. Run it with the migration role. Credentials are printed as a Markdown table on stdout.
func seedDemo(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: tenantku seed-demo")
	}
	if os.Getenv("DATABASE_URL") == "" {
		return errors.New("DATABASE_URL is required to seed demo data (use the migration role)")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return errors.New("could not connect to PostgreSQL to seed demo data")
	}
	defer pool.Close()
	seeder := seeder{pool: pool, store: events.Store{DB: pool}, categories: map[string]string{}}
	return seeder.run(ctx)
}

type seeder struct {
	pool       *pgxpool.Pool
	store      events.Store
	categories map[string]string
	hashes     map[string][]byte
}

func (s *seeder) hash(password string) ([]byte, error) {
	if s.hashes == nil {
		s.hashes = map[string][]byte{}
	}
	if h, ok := s.hashes[password]; ok {
		return h, nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	s.hashes[password] = h
	return h, err
}

// user creates or updates a verified account with a known password.
func (s *seeder) user(ctx context.Context, tx pgx.Tx, email, password string) (string, error) {
	hash, err := s.hash(password)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO users (email, password_hash, email_verified_at) VALUES ($1, $2, now())
		ON CONFLICT (email) DO UPDATE SET password_hash = EXCLUDED.password_hash,
			email_verified_at = COALESCE(users.email_verified_at, now())
		RETURNING id::text`, email, hash).Scan(&id)
	return id, err
}

func (s *seeder) run(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT name, id::text FROM business_categories`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			return err
		}
		s.categories[name] = id
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}

	if err := s.holdPolicy(ctx); err != nil {
		return err
	}
	if err := s.admin(ctx); err != nil {
		return err
	}
	today := time.Now().In(time.FixedZone("WIB", 7*3600))
	type created struct {
		event demoEvent
		id    string
		isNew bool
	}
	var made []created
	organizerRows := [][]string{}
	eventIDs := map[string]string{}
	for _, event := range demoEvents() {
		actor, org, err := s.organizer(ctx, event)
		if err != nil {
			return fmt.Errorf("organizer %s: %w", event.organizerEmail, err)
		}
		var id string
		err = s.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE slug = $1`, event.slug).Scan(&id)
		isNew := errors.Is(err, pgx.ErrNoRows)
		if isNew {
			blocks := make([]events.SeedBlock, len(event.blocks))
			for i, b := range event.blocks {
				blocks[i] = events.SeedBlock{Label: b.label, Stands: b.stands, Price: b.price}
			}
			id, _, err = s.store.SeedPublished(ctx, actor, org, events.SeedEvent{
				Name: event.name, Slug: event.slug, City: event.city, Address: event.address, Description: event.description,
				Instagram: event.instagram, Terms: demoTerms, Facilities: event.facilities, StartsAt: event.startsAt(today),
				EndsAt: event.endsAt(today), Blocks: blocks, Curated: event.curated,
			})
		}
		if err != nil {
			return fmt.Errorf("event %s: %w", event.name, err)
		}
		eventIDs[event.slug] = id
		made = append(made, created{event, id, isNew})
		stands := 0
		for _, b := range event.blocks {
			stands += b.stands
		}
		organizerRows = append(organizerRows, []string{event.organizerEmail, demoOrganizerPassword, event.organizer, event.name, event.city,
			fmt.Sprintf("%d", stands), event.instagram, id})
	}

	tenantRows := [][]string{}
	for i, tenant := range demoTenants() {
		email := fmt.Sprintf("tenant%02d@tenantku.com", i+1)
		if err := s.tenant(ctx, i, email, tenant, eventIDs); err != nil {
			return fmt.Errorf("tenant %s: %w", email, err)
		}
		note := ""
		if tenant.applyTo != "" {
			note = "sudah mengajukan ke " + tenant.applyTo
		}
		tenantRows = append(tenantRows, []string{email, demoTenantPassword, tenant.business, tenant.category, note})
	}

	photos, err := s.attachPhotos(ctx)
	if err != nil {
		return fmt.Errorf("product photos: %w", err)
	}
	slog.Info("product photos attached", "products", photos)

	fmt.Println("## Akun admin")
	fmt.Println("| Email | Password |\n| --- | --- |")
	fmt.Printf("| %s | %s |\n\n", demoAdminEmail, demoAdminPassword)
	fmt.Println("## Akun penyelenggara (satu akun, satu organisasi, satu acara aktif)")
	fmt.Println("| Email | Password | Organisasi | Acara | Kota | Stand | Instagram | ID acara |\n| --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, row := range organizerRows {
		fmt.Printf("| %s |\n", strings.Join(row, " | "))
	}
	fmt.Println("\n## Akun tenant (satu akun, satu usaha, ikut satu acara dalam satu waktu)")
	fmt.Println("| Email | Password | Usaha | Kategori | Catatan |\n| --- | --- | --- | --- | --- |")
	for _, row := range tenantRows {
		fmt.Printf("| %s |\n", strings.Join(row, " | "))
	}
	for _, m := range made {
		slog.Info("demo event", "name", m.event.name, "id", m.id, "created", m.isNew)
	}
	return nil
}

func (s *seeder) holdPolicy(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO hold_policy_versions (version, effective_at, platform_fee, provider_fee, refund_terms, refund_policy)
		VALUES ('demo-2026-1', '2026-01-01T00:00:00+07:00', 0, 0,
			'Jika penyelenggara membatalkan acara, tenant menerima refund 50% dari biaya sewa stand. Biaya admin pembayaran tidak dikembalikan.',
			'{"event_cancelled_by_organizer_percent": 50}'::jsonb)
		ON CONFLICT DO NOTHING`)
	return err
}

func (s *seeder) admin(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		id, err := s.user(ctx, tx, demoAdminEmail, demoAdminPassword)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO platform_admins (user_id, note, permissions) VALUES ($1, 'seed-demo', $2)
			ON CONFLICT (user_id) DO UPDATE SET permissions = EXCLUDED.permissions`, id, adminperm.All)
		return err
	})
}

// organizer returns the account and its one organization, creating both when missing.
func (s *seeder) organizer(ctx context.Context, event demoEvent) (actor, organization string, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if actor, err = s.user(ctx, tx, event.organizerEmail, demoOrganizerPassword); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO signup_profiles (user_id, role, phone, event_name) VALUES ($1, 'organizer', $2, $3)
			ON CONFLICT (user_id) DO UPDATE SET phone = EXCLUDED.phone, event_name = EXCLUDED.event_name`, actor, event.phone, event.name); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT id::text FROM organizations WHERE owner_id = $1`, actor).Scan(&organization)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.QueryRow(ctx, `INSERT INTO organizations (owner_id, name, verification_status, verified_at)
				VALUES ($1, $2, 'verified', now()) RETURNING id::text`, actor, event.organizer).Scan(&organization); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, 'owner')`, actor, organization)
		}
		return err
	})
	return actor, organization, err
}

func (s *seeder) tenant(ctx context.Context, index int, email string, tenant demoTenant, eventIDs map[string]string) error {
	category, ok := s.categories[tenant.category]
	if !ok {
		return fmt.Errorf("unknown category %q", tenant.category)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		actor, err := s.user(ctx, tx, email, demoTenantPassword)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO signup_profiles (user_id, role, phone) VALUES ($1, 'tenant', $2)
			ON CONFLICT (user_id) DO UPDATE SET phone = EXCLUDED.phone`, actor, fmt.Sprintf("+62812700%05d", 10000+index)); err != nil {
			return err
		}
		var business string
		err = tx.QueryRow(ctx, `SELECT id::text FROM businesses WHERE owner_id = $1`, actor).Scan(&business)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.QueryRow(ctx, `INSERT INTO businesses (owner_id, name, category_id) VALUES ($1, $2, $3) RETURNING id::text`,
				actor, tenant.business, category).Scan(&business); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO memberships (user_id, business_id, role) VALUES ($1, $2, 'owner')`, actor, business); err != nil {
				return err
			}
			// Data consent for the analytics features, recorded as the owner would give it.
			if _, err := tx.Exec(ctx, `INSERT INTO consent_events (business_id, actor_id, purpose, granted, policy_version)
				VALUES ($1, $2, 'analytics', true, 'demo-2026-1')`, business, actor); err != nil {
				return err
			}
			for _, product := range tenant.products {
				var id string
				if err := tx.QueryRow(ctx, `INSERT INTO products (business_id, name, price, cost, category_id, stock, created_by)
					VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
					business, product.name, product.price, product.cost, category, product.stock, actor).Scan(&id); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO inventory_movements (business_id, product_id, delta, reason, actor_id)
					VALUES ($1, $2, $3, 'initial', $4)`, business, id, product.stock, actor); err != nil {
					return err
				}
			}
		}
		if tenant.applyTo == "" {
			return nil
		}
		event, ok := eventIDs[tenant.applyTo]
		if !ok {
			return fmt.Errorf("unknown event %q", tenant.applyTo)
		}
		var application string
		err = tx.QueryRow(ctx, `INSERT INTO applications (event_id, business_id, submitted_by) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING RETURNING id::text`, event, business, actor).Scan(&application)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO application_status_history (application_id, actor_id, to_status) VALUES ($1, $2, 'submitted')`, application, actor)
		return err
	})
}

// attachPhotos gives every seeded product without a photo one from seed_assets, stored the way an upload is: a file in
// UPLOAD_DIR and a ready row in uploads owned by the business. A photo is shared by the products of one business that use it.
// Run it from the same directory as the API, or set UPLOAD_DIR to the API's value.
func (s *seeder) attachPhotos(ctx context.Context) (int, error) {
	dir := os.Getenv("UPLOAD_DIR")
	if dir == "" {
		dir = "data/uploads"
	}
	rows, err := s.pool.Query(ctx, `SELECT p.id::text, p.name, p.business_id::text, b.owner_id::text FROM products p
		JOIN businesses b ON b.id = p.business_id WHERE p.photo_upload_id IS NULL AND p.archived_at IS NULL ORDER BY p.created_at`)
	if err != nil {
		return 0, err
	}
	type product struct{ id, name, business, owner string }
	var todo []product
	for rows.Next() {
		var p product
		if err := rows.Scan(&p.id, &p.name, &p.business, &p.owner); err != nil {
			return 0, err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, rows.Err()
	}
	attached := 0
	for _, p := range todo {
		photo := photoFor(p.name)
		if photo == "" {
			continue
		}
		upload, err := s.photoUpload(ctx, dir, p.business, p.owner, photo)
		if err != nil {
			return attached, fmt.Errorf("%s: %w", p.name, err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE products SET photo_upload_id = $2 WHERE id = $1 AND photo_upload_id IS NULL`, p.id, upload); err != nil {
			return attached, err
		}
		attached++
	}
	return attached, nil
}

// photoUpload returns the ready upload of a seed photo for a business, creating the file and the row on first use.
func (s *seeder) photoUpload(ctx context.Context, dir, business, owner, photo string) (string, error) {
	name := strings.TrimSuffix(strings.TrimSuffix(photo, ".jpg"), ".webp")
	key := "product_photo/seed-" + strings.ReplaceAll(name, ".", "_") + "-" + business
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM uploads WHERE storage_key = $1`, key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	data, err := seedPhotos.ReadFile("seed_assets/" + photo)
	if err != nil {
		return "", err
	}
	contentType := http.DetectContentType(data)
	if contentType != "image/jpeg" && contentType != "image/webp" && contentType != "image/png" {
		return "", fmt.Errorf("%s is not a supported image (%s)", photo, contentType)
	}
	target := filepath.Join(dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return "", err
	}
	if err := os.WriteFile(target, data, 0o640); err != nil {
		return "", err
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO uploads (purpose, owner_type, owner_id, content_type, size_bytes, storage_key, status, created_by, expires_at, completed_at)
		VALUES ('product_photo', 'business', $1, $2, $3, $4, 'ready', $5, clock_timestamp(), clock_timestamp())
		ON CONFLICT (storage_key) DO UPDATE SET status = 'ready' RETURNING id::text`, business, contentType, len(data), key, owner).Scan(&id)
	return id, err
}
