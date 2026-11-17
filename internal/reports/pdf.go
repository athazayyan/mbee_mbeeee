package reports

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
)

var months = []string{"Jan", "Feb", "Mar", "Apr", "Mei", "Jun", "Jul", "Agu", "Sep", "Okt", "Nov", "Des"}

// rupiah memformat bilangan bulat seperti "Rp 1.250.000" (minus untuk nilai negatif).
func rupiah(value int64) string {
	sign := ""
	if value < 0 {
		sign, value = "-", -value
	}
	digits := strconv.FormatInt(value, 10)
	var out strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte('.')
		}
		out.WriteRune(r)
	}
	return sign + "Rp " + out.String()
}

func number(value int64) string {
	return strings.TrimPrefix(rupiah(value), "Rp ")
}

func dateIn(t time.Time, loc *time.Location) string {
	local := t.In(loc)
	return fmt.Sprintf("%d %s %d", local.Day(), months[local.Month()-1], local.Year())
}

func dateTimeIn(t time.Time, loc *time.Location) string {
	local := t.In(loc)
	return fmt.Sprintf("%s %02d.%02d", dateIn(t, loc), local.Hour(), local.Minute())
}

// renderPDF menyusun PDF rapor dari snapshot tersimpan. Snapshot tidak berubah, sehingga PDF yang sama
// dapat dibuat ulang kapan pun tanpa menyimpan berkas terpisah.
func renderPDF(snap Snapshot, code string) ([]byte, error) {
	loc, err := time.LoadLocation(snap.Timezone)
	if err != nil {
		loc = time.UTC
	}
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetTitle("Rapor usaha "+snap.BusinessName, true)
	pdf.SetCreator("TenantKu", true)
	pdf.SetMargins(18, 18, 18)
	pdf.SetAutoPageBreak(true, 18)
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	pdf.SetFooterFunc(func() {
		pdf.SetY(-12)
		pdf.SetFont("Helvetica", "", 8)
		pdf.SetTextColor(110, 110, 100)
		pdf.CellFormat(0, 5, tr(fmt.Sprintf("Kode verifikasi %s · Halaman %d", code, pdf.PageNo())), "", 0, "C", false, 0, "")
	})
	pdf.AddPage()
	width, _ := pdf.GetPageSize()
	content := width - 36

	text := func(size float64, style, value string, height float64) {
		pdf.SetFont("Helvetica", style, size)
		pdf.MultiCell(content, height, tr(value), "", "L", false)
	}
	pdf.SetTextColor(37, 37, 31)
	text(9, "B", "RAPOR USAHA · TENANTKU", 5)
	text(20, "B", snap.BusinessName, 9)
	period := dateIn(snap.PeriodStart, loc) + " - " + dateIn(snap.PeriodEnd.Add(-time.Second), loc) + " (" + snap.Timezone + ")"
	if snap.EventName != nil {
		period += " · " + *snap.EventName
	}
	pdf.SetTextColor(102, 103, 92)
	text(10, "", period, 6)
	pdf.Ln(2)
	pdf.SetFillColor(255, 243, 195)
	pdf.SetTextColor(74, 56, 0)
	pdf.SetFont("Helvetica", "B", 9)
	pdf.MultiCell(content, 6, tr(snap.VerificationLabel), "", "L", true)
	pdf.Ln(4)

	row := func(label, value string, bold bool) {
		style := ""
		if bold {
			style = "B"
		}
		pdf.SetFont("Helvetica", style, 10)
		pdf.SetTextColor(37, 37, 31)
		pdf.CellFormat(content*0.62, 7, tr(label), "B", 0, "L", false, 0, "")
		pdf.CellFormat(content*0.38, 7, tr(value), "B", 1, "R", false, 0, "")
	}
	heading := func(value string) {
		pdf.Ln(3)
		pdf.SetFont("Helvetica", "B", 12)
		pdf.SetTextColor(37, 37, 31)
		pdf.CellFormat(content, 8, tr(value), "", 1, "L", false, 0, "")
	}
	pdf.SetDrawColor(227, 227, 217)

	heading("Ringkasan penjualan")
	row("Penjualan bruto", rupiah(snap.GrossSales), false)
	row("Potongan", "-"+rupiah(snap.Discounts), false)
	row("Void dan retur", "-"+rupiah(snap.Refunds), false)
	row("Penjualan bersih", rupiah(snap.NetSales), true)
	row("Transaksi", number(snap.Transactions), false)
	if snap.AverageTransaction != nil {
		row("Rata-rata per transaksi", rupiah(*snap.AverageTransaction), false)
	}
	row("Unit terjual bersih", number(snap.UnitsNet), false)
	row("Tunai", rupiah(snap.CashSales), false)
	row("Non-tunai (belum diverifikasi)", rupiah(snap.NonCashSales), false)

	heading("Laba")
	if snap.CostComplete && snap.CostOfGoods != nil && snap.GrossProfit != nil && snap.OperatingProfit != nil {
		row("HPP", rupiah(*snap.CostOfGoods), false)
		row("Laba kotor", rupiah(*snap.GrossProfit), false)
		row("Pengeluaran", rupiah(snap.Expenses), false)
		row("Laba operasional", rupiah(*snap.OperatingProfit), true)
	} else {
		row("Pengeluaran", rupiah(snap.Expenses), false)
		pdf.SetTextColor(102, 103, 92)
		text(9, "", "Laba tidak dihitung karena modal (HPP) sebagian produk belum diisi.", 5)
	}

	if len(snap.Daily) > 0 {
		heading("Pola harian")
		pdf.SetFont("Helvetica", "B", 9)
		pdf.SetTextColor(102, 103, 92)
		pdf.CellFormat(content*0.4, 6, "Tanggal", "B", 0, "L", false, 0, "")
		pdf.CellFormat(content*0.35, 6, tr("Penjualan bersih"), "B", 0, "R", false, 0, "")
		pdf.CellFormat(content*0.25, 6, "Transaksi", "B", 1, "R", false, 0, "")
		pdf.SetFont("Helvetica", "", 9)
		pdf.SetTextColor(37, 37, 31)
		for _, day := range snap.Daily {
			label := day.Date
			if parsed, err := time.Parse("2006-01-02", day.Date); err == nil {
				label = dateIn(parsed, time.UTC)
			}
			pdf.CellFormat(content*0.4, 6, label, "B", 0, "L", false, 0, "")
			pdf.CellFormat(content*0.35, 6, tr(rupiah(day.NetSales)), "B", 0, "R", false, 0, "")
			pdf.CellFormat(content*0.25, 6, number(day.Transactions), "B", 1, "R", false, 0, "")
		}
	}

	if len(snap.TopProducts) > 0 {
		heading("Produk terlaris")
		for i, product := range snap.TopProducts {
			row(fmt.Sprintf("%d. %s (%s unit)", i+1, product.Name, number(product.Units)), rupiah(product.NetSales), false)
		}
	}

	pdf.Ln(5)
	pdf.SetTextColor(102, 103, 92)
	text(8, "", "Sumber: "+snap.Source, 4.5)
	text(8, "", "Kelengkapan: "+snap.Completeness.Note, 4.5)
	text(8, "", "Data sampai "+dateTimeIn(snap.CutoffAt, loc)+" · dibuat "+dateTimeIn(snap.GeneratedAt, loc)+" · kode verifikasi "+code, 4.5)

	var out bytes.Buffer
	if err := pdf.Output(&out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
