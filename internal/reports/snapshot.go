// Package reports membuat rapor usaha (E14): snapshot periode dari penjualan kasir yang tersinkron,
// serta tautan berbagi berbatas waktu yang tokennya hanya disimpan sebagai hash.
package reports

import (
	"math"
	"slices"
	"time"
)

const (
	verificationLabel = "Belum diverifikasi pihak ketiga; bukan skor kredit."
	sourceLabel       = "Kasir TenantKu, transaksi yang sudah tersinkron ke server."
)

type DailyTotal struct {
	Date         string `json:"date"`
	NetSales     int64  `json:"net_sales"`
	Transactions int64  `json:"transactions"`
}

type ProductTotal struct {
	Name     string `json:"name"`
	Units    int64  `json:"units"`
	NetSales int64  `json:"net_sales"`
}

type Completeness struct {
	Devices      int64      `json:"devices"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	Note         string     `json:"note"`
}

// Snapshot mengikuti definisi laporan PRD 11.3. Laba hanya diisi bila HPP seluruh barang terjual tersedia.
type Snapshot struct {
	BusinessName       string         `json:"business_name"`
	EventName          *string        `json:"event_name,omitempty"`
	PeriodStart        time.Time      `json:"period_start"`
	PeriodEnd          time.Time      `json:"period_end"`
	Timezone           string         `json:"timezone"`
	GeneratedAt        time.Time      `json:"generated_at"`
	CutoffAt           time.Time      `json:"cutoff_at"`
	Source             string         `json:"source"`
	VerificationStatus string         `json:"verification_status"`
	VerificationLabel  string         `json:"verification_label"`
	GrossSales         int64          `json:"gross_sales"`
	Discounts          int64          `json:"discounts"`
	Refunds            int64          `json:"refunds"`
	NetSales           int64          `json:"net_sales"`
	Transactions       int64          `json:"transaction_count"`
	VoidedTransactions int64          `json:"voided_transactions"`
	ReturnTransactions int64          `json:"return_transactions"`
	UnitsSold          int64          `json:"units_sold"`
	UnitsNet           int64          `json:"units_net"`
	AverageTransaction *int64         `json:"average_transaction,omitempty"`
	CostComplete       bool           `json:"cost_complete"`
	CostOfGoods        *int64         `json:"cost_of_goods,omitempty"`
	GrossProfit        *int64         `json:"gross_profit,omitempty"`
	Expenses           int64          `json:"expenses"`
	OperatingProfit    *int64         `json:"operating_profit,omitempty"`
	CashSales          int64          `json:"cash_sales"`
	NonCashSales       int64          `json:"non_cash_sales"`
	Daily              []DailyTotal   `json:"daily"`
	TopProducts        []ProductTotal `json:"top_products"`
	Completeness       Completeness   `json:"completeness"`
}

type saleRow struct {
	ID       string
	Day      string
	Total    int64
	Discount int64
	Method   string
	Refunded int64
	Voided   bool
	Returned bool
}

type itemRow struct {
	SaleID    string
	ProductID string
	Name      string
	Quantity  int64
	UnitPrice int64
	UnitCost  *int64
	Discount  int64
	Subtotal  int64
	Returned  int64
}

// aggregate menghitung metrik rapor dari baris penjualan dan item. Fungsi murni agar mudah diuji.
func aggregate(sales []saleRow, items []itemRow, expenses int64) Snapshot {
	var s Snapshot
	s.Expenses = expenses
	s.CostComplete = true
	itemsBySale := make(map[string][]itemRow, len(sales))
	for _, item := range items {
		itemsBySale[item.SaleID] = append(itemsBySale[item.SaleID], item)
	}
	daily := make(map[string]*DailyTotal)
	products := make(map[string]*ProductTotal)
	var cost int64
	for _, sale := range sales {
		net := sale.Total - sale.Refunded
		s.NetSales += net
		s.Refunds += sale.Refunded
		s.Discounts += sale.Discount
		if sale.Voided {
			s.VoidedTransactions++
		} else if sale.Returned {
			s.ReturnTransactions++
		}
		if sale.Method == "cash" {
			s.CashSales += net
		} else {
			s.NonCashSales += net
		}
		kept := false
		saleItems := itemsBySale[sale.ID]
		var gross int64
		for _, item := range saleItems {
			gross += item.Subtotal
		}
		for _, item := range saleItems {
			s.GrossSales += item.UnitPrice * item.Quantity
			s.Discounts += item.Discount
			s.UnitsSold += item.Quantity
			remaining := item.Quantity - item.Returned
			s.UnitsNet += remaining
			if remaining > 0 {
				kept = true
				if item.UnitCost == nil {
					s.CostComplete = false
				} else {
					cost += *item.UnitCost * remaining
				}
			}
			product := products[item.ProductID]
			if product == nil {
				product = &ProductTotal{Name: item.Name}
				products[item.ProductID] = product
			}
			product.Units += remaining
			// Bagian penjualan bersih barang ini setelah potongan transaksi, dikurangi bagian yang dikembalikan.
			if gross > 0 && item.Quantity > 0 {
				share := float64(item.Subtotal) / float64(gross) * float64(sale.Total)
				product.NetSales += int64(math.Round(share * float64(remaining) / float64(item.Quantity)))
			}
		}
		day := daily[sale.Day]
		if day == nil {
			day = &DailyTotal{Date: sale.Day}
			daily[sale.Day] = day
		}
		day.NetSales += net
		// Transaksi yang semua barangnya sudah di-void atau diretur tidak dihitung (PRD 11.3).
		if kept {
			s.Transactions++
			day.Transactions++
		}
	}
	if s.Transactions > 0 {
		average := s.NetSales / s.Transactions
		s.AverageTransaction = &average
	}
	if s.CostComplete {
		profit := s.NetSales - cost
		operating := profit - expenses
		s.CostOfGoods, s.GrossProfit, s.OperatingProfit = &cost, &profit, &operating
	}
	s.Daily = make([]DailyTotal, 0, len(daily))
	for _, day := range daily {
		s.Daily = append(s.Daily, *day)
	}
	slices.SortFunc(s.Daily, func(a, b DailyTotal) int {
		if a.Date < b.Date {
			return -1
		}
		if a.Date > b.Date {
			return 1
		}
		return 0
	})
	s.TopProducts = make([]ProductTotal, 0, len(products))
	for _, product := range products {
		if product.Units > 0 {
			s.TopProducts = append(s.TopProducts, *product)
		}
	}
	slices.SortFunc(s.TopProducts, func(a, b ProductTotal) int {
		if a.Units != b.Units {
			return int(b.Units - a.Units)
		}
		return int(b.NetSales - a.NetSales)
	})
	if len(s.TopProducts) > 5 {
		s.TopProducts = s.TopProducts[:5]
	}
	return s
}
