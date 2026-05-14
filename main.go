package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

var db *sql.DB

const defaultWarehouse = "WH-001"

func main() {
	var err error
	db, err = sql.Open("sqlite", "inv_grn.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	initDB()

	http.HandleFunc("/", serveForm)
	http.HandleFunc("/api/grn", handleGRN)
	http.HandleFunc("/api/gin", handleGIN)
	http.HandleFunc("/api/gin/", lookupStock)
	http.HandleFunc("/api/item/", lookupItem)
	http.HandleFunc("/api/onhand", getOnHand)

	log.Println("App running on http://localhost:9090")
	log.Fatal(http.ListenAndServe(":9090", nil))
}

func initDB() {
	schema := `
	CREATE TABLE IF NOT EXISTS inv_item_master (
		item_id TEXT PRIMARY KEY,
		generic_name TEXT NOT NULL,
		brand_name TEXT NOT NULL,
		dosage_form TEXT NOT NULL,
		strength TEXT NOT NULL,
		pack_size INTEGER NOT NULL,
		uom TEXT NOT NULL,
		barcode TEXT UNIQUE NOT NULL
	);

	CREATE TABLE IF NOT EXISTS inv_grn_header (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		grn_no TEXT NOT NULL,
		warehouse_id TEXT NOT NULL DEFAULT 'WH-001',
		received_date TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS inv_grn_detail (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		grn_header_id INTEGER NOT NULL,
		item_id TEXT NOT NULL,
		item_name TEXT NOT NULL,
		batch_no TEXT NOT NULL,
		expiry_date TEXT NOT NULL,
		qty INTEGER NOT NULL,
		uom TEXT NOT NULL,
		warehouse_id TEXT NOT NULL DEFAULT 'WH-001',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		FOREIGN KEY (grn_header_id) REFERENCES inv_grn_header(id)
	);

	CREATE TABLE IF NOT EXISTS inv_gin_header (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		gin_no TEXT NOT NULL,
		customer TEXT NOT NULL,
		warehouse_id TEXT NOT NULL DEFAULT 'WH-001',
		gin_date TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS inv_gin_detail (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		gin_header_id INTEGER NOT NULL,
		item_id TEXT NOT NULL,
		item_name TEXT NOT NULL,
		qty_issued INTEGER NOT NULL,
		batch_no TEXT NOT NULL,
		uom TEXT NOT NULL,
		warehouse_id TEXT NOT NULL DEFAULT 'WH-001',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		FOREIGN KEY (gin_header_id) REFERENCES inv_gin_header(id)
	);

	CREATE TABLE IF NOT EXISTS inv_onhand (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		item_id TEXT NOT NULL,
		item_name TEXT NOT NULL,
		warehouse_id TEXT NOT NULL DEFAULT 'WH-001',
		batch_no TEXT NOT NULL,
		expiry_date TEXT NOT NULL,
		qty_onhand INTEGER NOT NULL DEFAULT 0,
		uom TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(item_id, warehouse_id, batch_no)
	);
	`
	if _, err := db.Exec(schema); err != nil {
		log.Fatal(err)
	}

	// Seed item master
	items := []struct {
		ItemID, Generic, Brand, Form, Strength, UOM, Barcode string
		PackSize                                             int
	}{
		{"ITM-001", "Paracetamol", "Panadol", "Tablet", "500mg", "Pack", "8901234567890", 100},
		{"ITM-002", "Paracetamol", "Panadol", "Tablet", "250mg", "Pack", "8901234567891", 100},
		{"ITM-003", "Paracetamol", "Calpol", "Syrup", "120mg/5ml", "Bottle", "8901234567892", 60},
		{"ITM-004", "Paracetamol", "Crocin", "Tablet", "650mg", "Strip", "8901234567893", 15},
		{"ITM-005", "Paracetamol", "Tylenol", "Caplet", "500mg", "Pack", "8901234567894", 50},
	}
	for _, it := range items {
		db.Exec(
			`INSERT OR IGNORE INTO inv_item_master (item_id, generic_name, brand_name, dosage_form, strength, pack_size, uom, barcode)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			it.ItemID, it.Generic, it.Brand, it.Form, it.Strength, it.PackSize, it.UOM, it.Barcode,
		)
	}
	fmt.Println("Database initialized.")
}

func serveForm(w http.ResponseWriter, r *http.Request) {
	tmpl := template.Must(template.ParseFiles("templates/grn.html"))
	tmpl.Execute(w, nil)
}

type ItemMaster struct {
	ItemID      string `json:"item_id"`
	GenericName string `json:"generic_name"`
	BrandName   string `json:"brand_name"`
	DosageForm  string `json:"dosage_form"`
	Strength    string `json:"strength"`
	PackSize    int    `json:"pack_size"`
	UOM         string `json:"uom"`
	Barcode     string `json:"barcode"`
}

func lookupItem(w http.ResponseWriter, r *http.Request) {
	barcode := r.URL.Query().Get("barcode")
	if barcode == "" {
		http.Error(w, `{"error":"no barcode"}`, 400)
		return
	}

	var it ItemMaster
	err := db.QueryRow(
		`SELECT item_id, generic_name, brand_name, dosage_form, strength, pack_size, uom, barcode
		 FROM inv_item_master WHERE barcode = ?`, barcode,
	).Scan(&it.ItemID, &it.GenericName, &it.BrandName, &it.DosageForm, &it.Strength, &it.PackSize, &it.UOM, &it.Barcode)

	w.Header().Set("Content-Type", "application/json")
	if err == sql.ErrNoRows {
		json.NewEncoder(w).Encode(map[string]string{"error": "item not found"})
		return
	}
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"error": "lookup failed"})
		return
	}
	json.NewEncoder(w).Encode(it)
}

func handleGRN(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	itemID := r.FormValue("item_id")
	batchNo := r.FormValue("batch_no")
	expiryDate := r.FormValue("expiry_date")
	qtyStr := r.FormValue("qty")
	uom := r.FormValue("uom")
	warehouseID := r.FormValue("warehouse_id")
	if warehouseID == "" {
		warehouseID = defaultWarehouse
	}

	qty, err := strconv.Atoi(qtyStr)
	if err != nil || qty <= 0 {
		http.Error(w, "Invalid quantity", 400)
		return
	}

	if itemID == "" || batchNo == "" || expiryDate == "" {
		http.Error(w, "All fields required", 400)
		return
	}

	itemName := itemID
	genName := ""
	brandName := ""
	var packSize int
	db.QueryRow(`SELECT generic_name, brand_name, pack_size FROM inv_item_master WHERE item_id = ?`, itemID).Scan(&genName, &brandName, &packSize)
	if brandName != "" {
		itemName = fmt.Sprintf("%s (%s %s)", brandName, genName, r.FormValue("strength"))
	}
	if uom == "" {
		uom = "Pack"
	}

	tx, err := db.Begin()
	if err != nil {
		http.Error(w, "DB error", 500)
		return
	}
	defer tx.Rollback()

	grnNo := fmt.Sprintf("GRN-%s", time.Now().Format("20060102-150405"))

	res, err := tx.Exec(
		`INSERT INTO inv_grn_header (grn_no, warehouse_id, received_date) VALUES (?, ?, date('now'))`,
		grnNo, warehouseID,
	)
	if err != nil {
		http.Error(w, "Failed to create GRN header: "+err.Error(), 500)
		return
	}
	headerID, _ := res.LastInsertId()

	_, err = tx.Exec(
		`INSERT INTO inv_grn_detail (grn_header_id, item_id, item_name, batch_no, expiry_date, qty, uom, warehouse_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		headerID, itemID, itemName, batchNo, expiryDate, qty, uom, warehouseID,
	)
	if err != nil {
		http.Error(w, "Failed to create GRN detail: "+err.Error(), 500)
		return
	}

	_, err = tx.Exec(
		`INSERT INTO inv_onhand (item_id, item_name, warehouse_id, batch_no, expiry_date, qty_onhand, uom)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(item_id, warehouse_id, batch_no) DO UPDATE SET
		   qty_onhand = qty_onhand + excluded.qty_onhand,
		   updated_at = datetime('now')`,
		itemID, itemName, warehouseID, batchNo, expiryDate, qty, uom,
	)
	if err != nil {
		http.Error(w, "Failed to update onhand: "+err.Error(), 500)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, "Commit failed", 500)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(fmt.Sprintf(`
		<div class="success">
			<h2>GRN Created Successfully</h2>
			<table>
				<tr><td>GRN No</td><td>%s</td></tr>
				<tr><td>Warehouse</td><td>%s</td></tr>
				<tr><td>Item</td><td>%s</td></tr>
				<tr><td>Batch</td><td>%s</td></tr>
				<tr><td>Expiry</td><td>%s</td></tr>
				<tr><td>Quantity</td><td>%d %s</td></tr>
			</table>
			<button onclick="resetForm()">New GRN</button>
		</div>`, grnNo, warehouseID, itemName, batchNo, expiryDate, qty, uom)))
}

// --- GIN (Goods Issue Note) with FEFO ---

func lookupStock(w http.ResponseWriter, r *http.Request) {
	itemID := r.URL.Query().Get("item_id")
	warehouseID := r.URL.Query().Get("warehouse_id")
	if warehouseID == "" {
		warehouseID = defaultWarehouse
	}
	if itemID == "" {
		http.Error(w, `{"error":"missing item_id"}`, 400)
		return
	}

	rows, err := db.Query(`
		SELECT batch_no, expiry_date, qty_onhand, uom
		FROM inv_onhand
		WHERE item_id = ? AND warehouse_id = ? AND qty_onhand > 0
		ORDER BY expiry_date ASC
	`, itemID, warehouseID)
	if err != nil {
		http.Error(w, "Query failed", 500)
		return
	}
	defer rows.Close()

	type Batch struct {
		BatchNo    string `json:"batch_no"`
		ExpiryDate string `json:"expiry_date"`
		QtyOnHand  int    `json:"qty_onhand"`
		UOM        string `json:"uom"`
	}

	var batches []Batch
	total := 0
	for rows.Next() {
		var b Batch
		rows.Scan(&b.BatchNo, &b.ExpiryDate, &b.QtyOnHand, &b.UOM)
		batches = append(batches, b)
		total += b.QtyOnHand
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"item_id":      itemID,
		"warehouse_id": warehouseID,
		"total_avail":  total,
		"batches":      batches,
	})
}

func handleGIN(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	customer := r.FormValue("customer")
	itemID := r.FormValue("item_id")
	qtyStr := r.FormValue("qty")
	warehouseID := r.FormValue("warehouse_id")
	if warehouseID == "" {
		warehouseID = defaultWarehouse
	}

	qtyReq, err := strconv.Atoi(qtyStr)
	if err != nil || qtyReq <= 0 {
		http.Error(w, "Invalid quantity", 400)
		return
	}

	if customer == "" || itemID == "" {
		http.Error(w, "Customer and item required", 400)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		http.Error(w, "DB error", 500)
		return
	}
	defer tx.Rollback()

	// FEFO: fetch batches ordered by expiry ASC
	rows, err := tx.Query(`
		SELECT batch_no, expiry_date, qty_onhand, uom
		FROM inv_onhand
		WHERE item_id = ? AND warehouse_id = ? AND qty_onhand > 0
		ORDER BY expiry_date ASC
	`, itemID, warehouseID)
	if err != nil {
		http.Error(w, "Query failed", 500)
		return
	}
	defer rows.Close()

	type batch struct {
		BatchNo, ExpiryDate, UOM string
		Qty                      int
	}
	var batches []batch
	totalAvail := 0
	for rows.Next() {
		var b batch
		rows.Scan(&b.BatchNo, &b.ExpiryDate, &b.Qty, &b.UOM)
		batches = append(batches, b)
		totalAvail += b.Qty
	}
	rows.Close()

	if totalAvail < qtyReq {
		http.Error(w, fmt.Sprintf("Insufficient stock: requested %d, available %d", qtyReq, totalAvail), 400)
		return
	}

	// Build item name
	itemName := itemID
	genName := ""
	brandName := ""
	var packSize int
	var itemUOM string
	db.QueryRow(`SELECT generic_name, brand_name, pack_size, uom FROM inv_item_master WHERE item_id = ?`, itemID).Scan(&genName, &brandName, &packSize, &itemUOM)
	if brandName != "" {
		itemName = fmt.Sprintf("%s (%s %s)", brandName, genName, r.FormValue("strength"))
	}
	if itemUOM == "" {
		itemUOM = "Pack"
	}

	// FEFO allocation
	remaining := qtyReq
	type alloc struct{ BatchNo, ExpiryDate, UOM string; Qty int }
	var allocations []alloc
	for i := range batches {
		if remaining <= 0 {
			break
		}
		take := batches[i].Qty
		if take > remaining {
			take = remaining
		}
		remaining -= take
		allocations = append(allocations, alloc{
			BatchNo: batches[i].BatchNo, ExpiryDate: batches[i].ExpiryDate,
			UOM: batches[i].UOM, Qty: take,
		})
	}

	// Insert GIN header
	ginNo := fmt.Sprintf("GIN-%s", time.Now().Format("20060102-150405"))
	res, err := tx.Exec(
		`INSERT INTO inv_gin_header (gin_no, customer, warehouse_id, gin_date) VALUES (?, ?, ?, date('now'))`,
		ginNo, customer, warehouseID,
	)
	if err != nil {
		http.Error(w, "Failed to create GIN header", 500)
		return
	}
	headerID, _ := res.LastInsertId()

	// Insert GIN detail and deduct onhand
	for _, a := range allocations {
		_, err = tx.Exec(
			`INSERT INTO inv_gin_detail (gin_header_id, item_id, item_name, qty_issued, batch_no, uom, warehouse_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			headerID, itemID, itemName, a.Qty, a.BatchNo, a.UOM, warehouseID,
		)
		if err != nil {
			http.Error(w, "Failed to create GIN detail", 500)
			return
		}

		_, err = tx.Exec(
			`UPDATE inv_onhand SET qty_onhand = qty_onhand - ?, updated_at = datetime('now')
			 WHERE item_id = ? AND warehouse_id = ? AND batch_no = ?`,
			a.Qty, itemID, warehouseID, a.BatchNo,
		)
		if err != nil {
			http.Error(w, "Failed to deduct onhand", 500)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, "Commit failed", 500)
		return
	}

	// Build allocation summary
	var allocHTML string
	for _, a := range allocations {
		allocHTML += fmt.Sprintf("<tr><td>%s</td><td>%s</td><td>%d %s</td></tr>", a.BatchNo, a.ExpiryDate, a.Qty, a.UOM)
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(fmt.Sprintf(`
		<div class="success">
			<h2>GIN Created Successfully</h2>
			<table>
				<tr><td>GIN No</td><td>%s</td></tr>
				<tr><td>Customer</td><td>%s</td></tr>
				<tr><td>Warehouse</td><td>%s</td></tr>
				<tr><td>Item</td><td>%s</td></tr>
				<tr><td>Total Issued</td><td>%d %s</td></tr>
			</table>
			<h3 style="margin-top:12px;font-size:14px;color:#666;">FEFO Allocation</h3>
			<table>
				<tr><th style="text-align:left;padding:4px;">Batch</th><th style="text-align:left;padding:4px;">Expiry</th><th style="text-align:left;padding:4px;">Qty</th></tr>
				%s
			</table>
			<button onclick="resetForm()">New GIN</button>
		</div>`, ginNo, customer, warehouseID, itemName, qtyReq, itemUOM, allocHTML)))
}

// --- On Hand ---

func getOnHand(w http.ResponseWriter, r *http.Request) {
	warehouseID := r.URL.Query().Get("warehouse_id")
	filter := ""
	args := []interface{}{}
	if warehouseID != "" {
		filter = "WHERE o.warehouse_id = ?"
		args = append(args, warehouseID)
	}

	rows, err := db.Query(`
		SELECT o.item_id, m.brand_name, m.generic_name, m.strength, m.dosage_form,
			   o.batch_no, o.expiry_date, o.qty_onhand, o.uom, o.warehouse_id, o.updated_at
		FROM inv_onhand o
		LEFT JOIN inv_item_master m ON o.item_id = m.item_id
		`+filter+`
		ORDER BY m.brand_name, o.expiry_date
	`, args...)
	if err != nil {
		http.Error(w, "Query failed", 500)
		return
	}
	defer rows.Close()

	type OH struct {
		ItemID      string `json:"item_id"`
		BrandName   string `json:"brand_name"`
		GenericName string `json:"generic_name"`
		Strength    string `json:"strength"`
		DosageForm  string `json:"dosage_form"`
		BatchNo     string `json:"batch_no"`
		ExpiryDate  string `json:"expiry_date"`
		QtyOnHand   int    `json:"qty_onhand"`
		UOM         string `json:"uom"`
		WarehouseID string `json:"warehouse_id"`
		UpdatedAt   string `json:"updated_at"`
	}

	var results []OH
	for rows.Next() {
		var o OH
		rows.Scan(&o.ItemID, &o.BrandName, &o.GenericName, &o.Strength, &o.DosageForm,
			&o.BatchNo, &o.ExpiryDate, &o.QtyOnHand, &o.UOM, &o.WarehouseID, &o.UpdatedAt)
		results = append(results, o)
	}
	if results == nil {
		results = []OH{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}
