package main

import (
	"strings"
	"time"
)

// Demo data for `tenantku seed-demo`: five events in Banda Aceh and Lhokseumawe, their organizers, and tenants.
// Names are invented. Dates are relative to the day the command runs so the events are always upcoming.

const (
	demoOrganizerPassword = "Penyelenggara123"
	demoTenantPassword    = "Tenant123456"
	demoAdminEmail        = "admin@tenantku.com"
	demoAdminPassword     = "admin123456"
)

type demoBlock struct {
	label  string
	stands int
	price  int64
}

type demoEvent struct {
	organizer, organizerEmail, phone string
	name, slug, city, address        string
	instagram, description           string
	facilities                       []string
	startDays, durationDays          int
	startHour, endHour               int
	curated                          bool
	blocks                           []demoBlock
}

const demoTerms = `1. Stand disewa per slot acara dan tidak dapat dipindahtangankan tanpa persetujuan penyelenggara.
2. Penyewa wajib menjaga kebersihan stand dan membawa pulang sampahnya.
3. Barang yang dijual harus legal dan sesuai kategori usaha yang didaftarkan.
4. Penyewa hadir paling lambat 2 jam sebelum acara dibuka untuk menata stand.
5. Penyelenggara dapat menutup stand yang melanggar ketentuan tanpa pengembalian biaya sewa.`

var demoFacilities = []string{"Listrik 450 watt", "Meja dan kursi", "Area parkir", "Mushalla", "Toilet umum"}

func demoEvents() []demoEvent {
	return []demoEvent{
		{
			organizer: "Komunitas Peunayong Kreatif", organizerEmail: "eo.peunayong@tenantku.com", phone: "+6281260010001",
			name: "Pasar Kreatif Peunayong 2026", slug: "pasar-kreatif-peunayong-2026", city: "Banda Aceh",
			address: "Jl. T. Panglima Polem, Peunayong, Kec. Kuta Alam, Banda Aceh", instagram: "peunayong.kreatif",
			description: "Tiga hari pasar kreatif di jantung Peunayong. Puluhan pelaku UMKM kuliner, kopi, kriya, dan fesyen Aceh berjualan berdampingan dengan musik akustik dan bazar buku.",
			facilities:  demoFacilities, startDays: 14, durationDays: 3, startHour: 10, endHour: 21,
			blocks: []demoBlock{{"Kuliner", 6, 350000}, {"Minuman dan Kopi", 4, 300000}, {"Kriya dan Fesyen", 4, 250000}},
		},
		{
			organizer: "Taman Sari Event Organizer", organizerEmail: "eo.tamansari@tenantku.com", phone: "+6281260010002",
			name: "Banda Aceh Night Market Taman Sari", slug: "banda-aceh-night-market-taman-sari", city: "Banda Aceh",
			address: "Taman Sari, Kec. Baiturrahman, Kota Banda Aceh", instagram: "nightmarket.bna",
			description: "Pasar malam akhir pekan dengan jajanan khas Aceh, lampu-lampu taman, dan panggung musik lokal. Dibuka sore hingga larut malam.",
			facilities:  append([]string{"Penerangan malam"}, demoFacilities...), startDays: 21, durationDays: 3, startHour: 17, endHour: 23,
			blocks: []demoBlock{{"Kuliner Malam", 8, 400000}, {"Jajanan", 6, 300000}, {"Kreatif", 4, 250000}},
		},
		{
			organizer: "Aceh Coffee Society", organizerEmail: "eo.acehcoffee@tenantku.com", phone: "+6281260010003",
			name: "Festival Kopi Ulee Kareng", slug: "festival-kopi-ulee-kareng", city: "Banda Aceh",
			address: "Lapangan Blang Padang, Kec. Baiturrahman, Kota Banda Aceh", instagram: "acehcoffeesociety", curated: true,
			description: "Festival kopi Aceh: sanger, kopi saring, dan kopi Gayo dari kedai dan petani lokal. Tenant diseleksi penyelenggara agar tiap kategori seimbang.",
			facilities:  demoFacilities, startDays: 28, durationDays: 3, startHour: 9, endHour: 20,
			blocks: []demoBlock{{"Kopi", 8, 450000}, {"Pendamping", 6, 300000}},
		},
		{
			organizer: "Forum UMKM Lhokseumawe", organizerEmail: "eo.umkmlsm@tenantku.com", phone: "+6281260010004",
			name: "Bazar UMKM Pantai Ujong Blang", slug: "bazar-umkm-pantai-ujong-blang", city: "Lhokseumawe",
			address: "Pantai Ujong Blang, Kec. Banda Sakti, Kota Lhokseumawe", instagram: "umkm.lhokseumawe",
			description: "Bazar akhir pekan di tepi pantai: kuliner laut, oleh-oleh khas Aceh Utara, dan fesyen lokal, dengan lomba layang-layang untuk keluarga.",
			facilities:  demoFacilities, startDays: 15, durationDays: 2, startHour: 9, endHour: 20,
			blocks: []demoBlock{{"Kuliner Pantai", 6, 275000}, {"Oleh-oleh", 6, 225000}, {"Fesyen", 3, 225000}},
		},
		{
			organizer: "Pasee Event Kreatif", organizerEmail: "eo.paseeevent@tenantku.com", phone: "+6281260010005",
			name: "Lhokseumawe Culinary Night Fest", slug: "lhokseumawe-culinary-night-fest", city: "Lhokseumawe",
			address: "Lapangan Hiraq, Kec. Banda Sakti, Kota Lhokseumawe", instagram: "lsm.culinaryfest",
			description: "Festival kuliner malam dengan menu Aceh dan Nusantara, minuman segar, dan panggung hiburan setiap malam.",
			facilities:  append([]string{"Penerangan malam"}, demoFacilities...), startDays: 35, durationDays: 3, startHour: 17, endHour: 23,
			blocks: []demoBlock{{"Nusantara", 8, 325000}, {"Aceh", 8, 325000}, {"Minuman", 4, 250000}},
		},
	}
}

func (e demoEvent) startsAt(today time.Time) time.Time {
	return time.Date(today.Year(), today.Month(), today.Day()+e.startDays, e.startHour, 0, 0, 0, today.Location())
}

func (e demoEvent) endsAt(today time.Time) time.Time {
	return time.Date(today.Year(), today.Month(), today.Day()+e.startDays+e.durationDays-1, e.endHour, 0, 0, 0, today.Location())
}

type demoProduct struct {
	name        string
	price, cost int64
	stock       int
}

// photoRules picks a seed photo (see seed_assets) for a product by words in its name. The first rule that matches wins.
var photoRules = []struct{ contains, photo string }{
	{"Mie Aceh", "mie_bakso.webp"}, {"Sanger", "sanger.jpg"}, {"Kopi Gayo", "kopi_gayo.jpg"}, {"Biji Kopi", "kopi_beans.jpg"},
	{"Cold Brew", "cold_brew.jpg"}, {"Kopi", "kopi_cup.jpg"}, {"Es Timun", "es_tabung.jpg"}, {"Es Kelapa", "es_tabung.jpg"},
	{"Boba", "boba.jpg"}, {"Thai Tea", "thai_tea.jpg"}, {"Kue Adee", "kue_adee.jpg"}, {"Bolu", "bolu.jpg"}, {"Kue Bhoi", "kue_bhoi.jpg"},
	{"Timphan", "timphan.jpg"}, {"Pulut", "pulut.jpg"}, {"Keumamah", "keumamah.jpg"}, {"Nasi Gurih", "nasi_gurih.jpg"},
	{"Sate", "sate.jpg"}, {"Lontong", "lontong.jpg"}, {"Martabak Telur", "martabak_telur.jpg"}, {"Martabak Manis", "martabak_manis.jpg"},
	{"Roti Cane", "roti_cane.jpg"}, {"Roti Selai", "roti_bakar.jpg"}, {"Wajeb", "wajeb.jpg"}, {"Dodol", "dodol.jpg"},
	{"Kuah Beulangong", "kuah_beulangong.jpg"}, {"Nasi Putih", "nasi_putih.jpg"}, {"Songket", "songket.jpg"},
	{"Peci", "topi_anyaman.jpg"}, {"Kupiah", "topi_anyaman.jpg"}, {"Kemeja Batik", "batik_kain.jpg"}, {"Kain Batik", "batik_kain.jpg"},
	{"Kaos", "kaos.jpg"}, {"Hijab", "hijab.jpg"}, {"Pashmina", "pashmina.jpg"}, {"Rencong", "rencong.jpg"},
	{"Gantungan", "gantungan.jpg"}, {"Tas Anyaman", "tas_anyaman.jpg"}, {"Tikar", "tikar.jpg"}, {"Topi Anyaman", "topi_anyaman.jpg"},
	{"Tempat Tisu", "tisu.jpg"}, {"Cangkir", "keramik_cangkir.jpg"}, {"Piring Hias", "keramik_piring.jpg"},
	{"Polaroid", "polaroid.jpg"}, {"Cetak Foto", "polaroid.jpg"}, {"Henna", "henna.jpg"}, {"Make Up", "makeup.jpg"}, {"Sablon", "sablon.jpg"},
	{"Magnet", "magnet.jpg"}, {"Pin ", "pin.jpg"}, {"Kaktus", "kaktus.jpg"}, {"Sukulen", "sukulen.jpg"}, {"Madu", "madu.jpg"},
	{"Komik", "buku_resep.jpg"}, {"Buku", "buku_resep.jpg"},
}

func photoFor(product string) string {
	for _, rule := range photoRules {
		if strings.Contains(product, rule.contains) {
			return rule.photo
		}
	}
	return ""
}

type demoTenant struct {
	business, category string
	products           []demoProduct
	applyTo            string // slug of a curated event this tenant has applied to
}

func p(name string, price, cost int64, stock int) demoProduct {
	return demoProduct{name, price, cost, stock}
}

const (
	food    = "Makanan dan minuman"
	fashion = "Fesyen"
	craft   = "Kerajinan"
	service = "Jasa"
	other   = "Lainnya"
)

func demoTenants() []demoTenant {
	coffee := "festival-kopi-ulee-kareng"
	return []demoTenant{
		{"Mie Aceh Bang Ucok", food, []demoProduct{p("Mie Aceh Goreng", 28000, 14000, 120), p("Mie Aceh Kuah", 28000, 14000, 100), p("Mie Aceh Kepiting", 55000, 32000, 40)}, ""},
		{"Kopi Sanger Seulanga", food, []demoProduct{p("Sanger Dingin", 15000, 6000, 200), p("Kopi Saring Panas", 12000, 4500, 200), p("Kopi Gayo Seduh", 20000, 9000, 80)}, coffee},
		{"Kue Adee Cut Nyak", food, []demoProduct{p("Kue Adee", 35000, 18000, 60), p("Bolu Meuseukat", 40000, 21000, 50), p("Kue Bhoi", 25000, 12000, 80)}, ""},
		{"Timphan Bu Cut", food, []demoProduct{p("Timphan Srikaya", 12000, 5000, 150), p("Timphan Pisang", 10000, 4000, 150), p("Pulut Panggang", 15000, 7000, 100)}, ""},
		{"Keumamah Dapoe Aceh", food, []demoProduct{p("Keumamah Kemasan", 45000, 25000, 60), p("Nasi Gurih Aceh", 25000, 12000, 80)}, ""},
		{"Sate Matang Pak Yusuf", food, []demoProduct{p("Sate Matang Sapi (10 tusuk)", 35000, 20000, 100), p("Sate Matang Kambing (10 tusuk)", 40000, 24000, 80), p("Lontong", 5000, 2000, 200)}, ""},
		{"Martabak Aceh Pasee", food, []demoProduct{p("Martabak Telur", 30000, 15000, 80), p("Martabak Manis Coklat Keju", 35000, 17000, 70)}, ""},
		{"Es Timun Serut Lhok", food, []demoProduct{p("Es Timun Serut", 10000, 3500, 250), p("Es Kelapa Muda", 15000, 7000, 150)}, ""},
		{"Roti Cane Kari Gampong", food, []demoProduct{p("Roti Cane Kuah Kari", 22000, 10000, 100), p("Roti Cane Susu Coklat", 18000, 8000, 100)}, ""},
		{"Kopi Ulee Kareng Meuligoe", food, []demoProduct{p("Kopi Ulee Kareng Panas", 10000, 3500, 250), p("Sanger Es", 14000, 5500, 200), p("Biji Kopi 250 gr", 55000, 33000, 50)}, coffee},
		{"Warkop Tikoe Lampulo", food, []demoProduct{p("Kopi Tarik", 13000, 5000, 200), p("Roti Selai Bakar", 12000, 5000, 100)}, coffee},
		{"Gayo Highland Bean", food, []demoProduct{p("Kopi Gayo Arabika 200 gr", 65000, 40000, 60), p("Cold Brew Gayo", 22000, 10000, 80)}, coffee},
		{"Wajeb Kampoeng Lhokseumawe", food, []demoProduct{p("Wajeb", 30000, 15000, 60), p("Dodol Aceh", 28000, 14000, 60)}, ""},
		{"Dapur Pasee Kuah Beulangong", food, []demoProduct{p("Kuah Beulangong Porsi", 30000, 16000, 100), p("Nasi Putih", 5000, 2000, 200)}, ""},
		{"Boba Lampineung", food, []demoProduct{p("Boba Brown Sugar", 18000, 7000, 150), p("Thai Tea", 15000, 5500, 150)}, ""},
		{"Songket Aceh Meuligoe", fashion, []demoProduct{p("Selendang Songket", 350000, 220000, 12), p("Peci Meukeutop", 120000, 70000, 25)}, ""},
		{"Batik Aceh Pucok Reudeuk", fashion, []demoProduct{p("Kemeja Batik Aceh", 225000, 130000, 20), p("Kain Batik Aceh 2 m", 275000, 160000, 15)}, ""},
		{"Kaos Haba Aceh", fashion, []demoProduct{p("Kaos Haba Aceh", 95000, 45000, 60), p("Topi Kupiah Aceh", 65000, 30000, 40)}, ""},
		{"Hijab Lampuuk", fashion, []demoProduct{p("Hijab Voal Motif Pintoe Aceh", 85000, 42000, 50), p("Pashmina Ceruty", 75000, 35000, 40)}, ""},
		{"Rencong Mini Kriya Aceh", craft, []demoProduct{p("Rencong Mini Souvenir", 75000, 35000, 40), p("Gantungan Kunci Pintoe Aceh", 15000, 5000, 120)}, ""},
		{"Anyaman Pandan Aceh Utara", craft, []demoProduct{p("Tas Anyaman Pandan", 120000, 60000, 25), p("Tikar Pandan Kecil", 85000, 40000, 20)}, ""},
		{"Tenun Pandan Blang Pidie", craft, []demoProduct{p("Topi Anyaman", 55000, 25000, 40), p("Tempat Tisu Anyaman", 45000, 20000, 40)}, ""},
		{"Keramik Gampong Raya", craft, []demoProduct{p("Cangkir Keramik Motif Aceh", 65000, 30000, 40), p("Piring Hias", 90000, 45000, 25)}, ""},
		{"Foto Polaroid Ulee Lheue", service, []demoProduct{p("Foto Polaroid Instan", 25000, 9000, 200), p("Cetak Foto Frame", 35000, 14000, 100)}, ""},
		{"Henna dan Make Up Dara Baro", service, []demoProduct{p("Henna Tangan", 50000, 15000, 80), p("Make Up Dasar", 150000, 50000, 30)}, ""},
		{"Sablon Kaos Kilat Banda", service, []demoProduct{p("Sablon Nama Kaos", 40000, 15000, 100)}, ""},
		{"Souvenir Gayo Mini", other, []demoProduct{p("Magnet Kulkas Aceh", 20000, 7000, 100), p("Pin Rumoh Aceh", 15000, 5000, 100)}, ""},
		{"Tanaman Hias Lamteumen", other, []demoProduct{p("Kaktus Mini Pot", 20000, 8000, 80), p("Sukulen Pot Kecil", 25000, 10000, 80)}, ""},
		{"Madu Hutan Aceh Tengah", other, []demoProduct{p("Madu Hutan 250 ml", 85000, 55000, 60), p("Madu Hutan 500 ml", 160000, 105000, 40)}, ""},
		{"Buku dan Komik Lokal Aceh", other, []demoProduct{p("Komik Hikayat Aceh", 55000, 28000, 50), p("Buku Resep Kuliner Aceh", 80000, 42000, 30)}, ""},
	}
}
