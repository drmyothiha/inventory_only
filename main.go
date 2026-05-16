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
	db, err = sql.Open("sqlite", "file:inv_grn.db?cache=shared&mode=rwc&_journal_mode=DELETE&_busy_timeout=5000")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	initDB()

	http.HandleFunc("/", serveForm)
	http.HandleFunc("/api/grn", handleGRN)
	http.HandleFunc("/api/gin", handleGIN)
	http.HandleFunc("/api/gin/", lookupStock)
	http.HandleFunc("/api/search-items", searchItems)
	http.HandleFunc("/api/item/", lookupItem)
	http.HandleFunc("/api/onhand", getOnHand)
	http.HandleFunc("/api/create-item", createItem)
	http.HandleFunc("/api/link-barcode", linkBarcode)

	log.Println("App running on http://localhost:9090")
	log.Fatal(http.ListenAndServe(":9090", nil))
}

func initDB() {
	// Force DELETE journal mode — WAL can cause readonly issues on some platforms
	db.Exec("PRAGMA journal_mode=DELETE")

	schema := `
	CREATE TABLE IF NOT EXISTS inv_item_master (
		item_id TEXT PRIMARY KEY,
		generic_name TEXT NOT NULL,
		brand_name TEXT NOT NULL,
		dosage_form TEXT NOT NULL,
		strength TEXT NOT NULL,
		pack_size INTEGER NOT NULL,
		uom TEXT NOT NULL,
		barcode TEXT UNIQUE NOT NULL,
		manufacturer TEXT NOT NULL DEFAULT '',
		country TEXT NOT NULL DEFAULT ''
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

	CREATE TABLE IF NOT EXISTS inv_batch_master (
		batch_id INTEGER PRIMARY KEY AUTOINCREMENT,
		item_id TEXT NOT NULL,
		batch_no TEXT NOT NULL,
		expiry_date TEXT NOT NULL,
		manufacture_date TEXT,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(item_id, batch_no)
	);

	CREATE TABLE IF NOT EXISTS inv_onhand (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		item_id TEXT NOT NULL,
		item_name TEXT NOT NULL,
		warehouse_id TEXT NOT NULL DEFAULT 'WH-001',
		batch_id INTEGER NOT NULL,
		qty_onhand INTEGER NOT NULL DEFAULT 0,
		uom TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(item_id, warehouse_id, batch_id),
		FOREIGN KEY (batch_id) REFERENCES inv_batch_master(batch_id)
	);

	CREATE TABLE IF NOT EXISTS inv_supplier_master (
		supplier_id TEXT PRIMARY KEY,
		supplier_name TEXT NOT NULL,
		contact_info TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS inv_contract_master (
		contract_no TEXT PRIMARY KEY,
		supplier_id TEXT NOT NULL,
		start_date TEXT NOT NULL,
		end_date TEXT NOT NULL,
		FOREIGN KEY (supplier_id) REFERENCES inv_supplier_master(supplier_id)
	);

	CREATE TABLE IF NOT EXISTS inv_pmed_header (
		pmed_no TEXT PRIMARY KEY,
		contract_no TEXT NOT NULL,
		supplier_id TEXT NOT NULL,
		pmed_date TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'OPEN',
		FOREIGN KEY (contract_no) REFERENCES inv_contract_master(contract_no),
		FOREIGN KEY (supplier_id) REFERENCES inv_supplier_master(supplier_id)
	);

	CREATE TABLE IF NOT EXISTS inv_pmed_detail (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		pmed_no TEXT NOT NULL,
		dsn TEXT NOT NULL,
		sku_no TEXT NOT NULL,
		qty_ordered INTEGER NOT NULL,
		uom TEXT NOT NULL,
		UNIQUE(pmed_no, dsn),
		FOREIGN KEY (pmed_no) REFERENCES inv_pmed_header(pmed_no)
	);

	CREATE TABLE IF NOT EXISTS inv_do_header (
		do_no TEXT PRIMARY KEY,
		pmed_no TEXT NOT NULL,
		supplier_id TEXT NOT NULL,
		do_date TEXT NOT NULL,
		received_date TEXT NOT NULL,
		FOREIGN KEY (pmed_no) REFERENCES inv_pmed_header(pmed_no),
		FOREIGN KEY (supplier_id) REFERENCES inv_supplier_master(supplier_id)
	);

	CREATE TABLE IF NOT EXISTS inv_do_detail (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		do_no TEXT NOT NULL,
		dsn TEXT NOT NULL,
		sku_no TEXT NOT NULL,
		qty_delivered INTEGER NOT NULL,
		batch_no TEXT NOT NULL,
		expiry_date TEXT NOT NULL,
		FOREIGN KEY (do_no) REFERENCES inv_do_header(do_no)
	);

	CREATE TABLE IF NOT EXISTS inv_store_location (
		location_id INTEGER PRIMARY KEY,
		location_name TEXT NOT NULL UNIQUE
	);
	`
	if _, err := db.Exec(schema); err != nil {
		log.Fatal(err)
	}

	// Migrate: add new columns if missing (ignore errors if already exist)
	db.Exec("ALTER TABLE inv_item_master ADD COLUMN manufacturer TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE inv_item_master ADD COLUMN country TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE inv_item_master ADD COLUMN sku_no TEXT NOT NULL DEFAULT ''")
	db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_item_master_sku ON inv_item_master(sku_no) WHERE sku_no != ''")

	// Seed store locations 1 to 23
	for i := 1; i <= 23; i++ {
		db.Exec("INSERT OR IGNORE INTO inv_store_location (location_id, location_name) VALUES (?, ?)", i, fmt.Sprintf("Store %d", i))
	}

	migrateFromLegacyOnhand()

	// Seed item master
	items := []struct {
		ItemID, Generic, Brand, Form, Strength, UOM, Barcode, Manufacturer, Country string
		PackSize                                                                    int
	}{
		{"ITM-001", "Paracetamol", "Panadol", "Tablet", "500mg", "Pack", "8901234567890", "GSK", "UK", 100},
		{"ITM-002", "Paracetamol", "Panadol", "Tablet", "250mg", "Pack", "8901234567891", "GSK", "UK", 100},
		{"ITM-003", "Paracetamol", "Calpol", "Syrup", "120mg/5ml", "Bottle", "8901234567892", "GSK", "UK", 60},
		{"ITM-004", "Paracetamol", "Crocin", "Tablet", "650mg", "Strip", "8901234567893", "GSK", "India", 15},
		{"ITM-005", "Paracetamol", "Tylenol", "Caplet", "500mg", "Pack", "8901234567894", "J&J", "USA", 50},
	}
	for _, it := range items {
		db.Exec(
			`INSERT OR IGNORE INTO inv_item_master (item_id, generic_name, brand_name, dosage_form, strength, pack_size, uom, barcode, manufacturer, country)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			it.ItemID, it.Generic, it.Brand, it.Form, it.Strength, it.PackSize, it.UOM, it.Barcode, it.Manufacturer, it.Country)
	}
	fmt.Println("Database initialized.")
}

// migrateFromLegacyOnhand detects old onhand schema (with batch_no/expiry_date columns)
// and migrates data to the normalized schema with inv_batch_master.
func migrateFromLegacyOnhand() {
	// Check if migration is needed by looking for the old 'batch_no' column
	rows, err := db.Query("PRAGMA table_info(inv_onhand)")
	if err != nil {
		return
	}
	defer rows.Close()

	hasBatchNo := false
	for rows.Next() {
		var cid, notnull int
		var name, coltype, dflt string
		var pk int
		rows.Scan(&cid, &name, &coltype, &notnull, &dflt, &pk)
		if name == "batch_no" {
			hasBatchNo = true
			break
		}
	}
	rows.Close()

	if !hasBatchNo {
		return // already migrated or fresh install
	}

	fmt.Println("Migrating inv_onhand to normalized batch_master schema...")

	tx, err := db.Begin()
	if err != nil {
		log.Fatal("Migration begin failed:", err)
	}
	defer tx.Rollback()

	// Populate batch_master from existing onhand
	_, err = tx.Exec(`
		INSERT OR IGNORE INTO inv_batch_master (item_id, batch_no, expiry_date)
		SELECT DISTINCT item_id, batch_no, expiry_date FROM inv_onhand
	`)
	if err != nil {
		log.Fatal("Migration populate batch_master failed:", err)
	}

	// Create new onhand table with batch_id
	_, err = tx.Exec(`
		CREATE TABLE inv_onhand_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			item_id TEXT NOT NULL,
			item_name TEXT NOT NULL,
			warehouse_id TEXT NOT NULL DEFAULT 'WH-001',
			batch_id INTEGER NOT NULL,
			qty_onhand INTEGER NOT NULL DEFAULT 0,
			uom TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(item_id, warehouse_id, batch_id),
			FOREIGN KEY (batch_id) REFERENCES inv_batch_master(batch_id)
		)
	`)
	if err != nil {
		log.Fatal("Migration create new onhand failed:", err)
	}

	// Migrate data
	_, err = tx.Exec(`
		INSERT INTO inv_onhand_new (id, item_id, item_name, warehouse_id, batch_id, qty_onhand, uom, updated_at)
		SELECT o.id, o.item_id, o.item_name, o.warehouse_id, b.batch_id, o.qty_onhand, o.uom, o.updated_at
		FROM inv_onhand o
		JOIN inv_batch_master b ON o.item_id = b.item_id AND o.batch_no = b.batch_no
	`)
	if err != nil {
		log.Fatal("Migration data copy failed:", err)
	}

	// Swap tables
	_, err = tx.Exec("DROP TABLE inv_onhand")
	if err != nil {
		log.Fatal("Migration drop old onhand failed:", err)
	}
	_, err = tx.Exec("ALTER TABLE inv_onhand_new RENAME TO inv_onhand")
	if err != nil {
		log.Fatal("Migration rename failed:", err)
	}

	if err := tx.Commit(); err != nil {
		log.Fatal("Migration commit failed:", err)
	}
	fmt.Println("Migration complete.")
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
	Barcode      string `json:"barcode"`
	Manufacturer string `json:"manufacturer"`
	Country      string `json:"country"`
}

func searchItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	allItems := r.URL.Query().Get("all") == "1"
	warehouseID := r.URL.Query().Get("warehouse_id")
	if warehouseID == "" {
		warehouseID = defaultWarehouse
	}
	if q == "" {
		http.Error(w, `{"error":"missing q"}`, 400)
		return
	}

	if allItems {
		like := "%" + q + "%"
		rows, err := db.Query(`
			SELECT item_id, generic_name, brand_name, dosage_form, strength, uom, barcode, pack_size, manufacturer, country
			FROM inv_item_master
			WHERE generic_name LIKE ? OR brand_name LIKE ?
			ORDER BY brand_name
			LIMIT 20
		`, like, like)
		if err != nil {
			http.Error(w, "Query failed", 500)
			return
		}
		defer rows.Close()

		type SimpleItem struct {
			ItemID      string `json:"item_id"`
			GenericName string `json:"generic_name"`
			BrandName   string `json:"brand_name"`
			DosageForm  string `json:"dosage_form"`
			Strength    string `json:"strength"`
			UOM         string `json:"uom"`
			Barcode     string `json:"barcode"`
			PackSize     int    `json:"pack_size"`
		Manufacturer string `json:"manufacturer"`
		Country      string `json:"country"`
		}
		var results []SimpleItem
		for rows.Next() {
			var it SimpleItem
			rows.Scan(&it.ItemID, &it.GenericName, &it.BrandName, &it.DosageForm, &it.Strength, &it.UOM, &it.Barcode, &it.PackSize, &it.Manufacturer, &it.Country)
			results = append(results, it)
		}
		if results == nil {
			results = []SimpleItem{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
		return
	}

	like := "%" + q + "%"
	rows, err := db.Query(`
		SELECT i.item_id, i.generic_name, i.brand_name, i.dosage_form, i.strength,
			   i.uom, i.barcode, b.batch_no, b.expiry_date, o.qty_onhand, o.item_name
		FROM inv_item_master i
		JOIN inv_onhand o ON i.item_id = o.item_id AND o.warehouse_id = ?
		JOIN inv_batch_master b ON o.batch_id = b.batch_id
		WHERE (i.generic_name LIKE ? OR i.brand_name LIKE ?) AND o.qty_onhand > 0
		ORDER BY i.brand_name, b.expiry_date ASC
	`, warehouseID, like, like)
	if err != nil {
		http.Error(w, "Query failed", 500)
		return
	}
	defer rows.Close()

	type Result struct {
		ItemID      string `json:"item_id"`
		GenericName string `json:"generic_name"`
		BrandName   string `json:"brand_name"`
		DosageForm  string `json:"dosage_form"`
		Strength    string `json:"strength"`
		UOM         string `json:"uom"`
		Barcode     string `json:"barcode"`
		BatchNo     string `json:"batch_no"`
		ExpiryDate  string `json:"expiry_date"`
		QtyOnHand   int    `json:"qty_onhand"`
		ItemName    string `json:"item_name"`
	}

	var results []Result
	for rows.Next() {
		var r Result
		rows.Scan(&r.ItemID, &r.GenericName, &r.BrandName, &r.DosageForm, &r.Strength,
			&r.UOM, &r.Barcode, &r.BatchNo, &r.ExpiryDate, &r.QtyOnHand, &r.ItemName)
		results = append(results, r)
	}
	if results == nil {
		results = []Result{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func lookupItem(w http.ResponseWriter, r *http.Request) {
	barcode := r.URL.Query().Get("barcode")
	if barcode == "" {
		http.Error(w, `{"error":"no barcode"}`, 400)
		return
	}

	var it ItemMaster
	err := db.QueryRow(
		`SELECT item_id, generic_name, brand_name, dosage_form, strength, pack_size, uom, barcode, manufacturer, country
		 FROM inv_item_master WHERE barcode = ?`, barcode,
	).Scan(&it.ItemID, &it.GenericName, &it.BrandName, &it.DosageForm, &it.Strength, &it.PackSize, &it.UOM, &it.Barcode, &it.Manufacturer, &it.Country)

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

	// Enforce: one batch_no = one expiry_date
	// If batch_no already exists with a different expiry, reject.
	var existingExpiry string
	err = tx.QueryRow(
		`SELECT expiry_date FROM inv_batch_master WHERE item_id = ? AND batch_no = ?`,
		itemID, batchNo,
	).Scan(&existingExpiry)
	if err == nil {
		// Batch exists — verify expiry matches
		if existingExpiry != expiryDate {
			http.Error(w, fmt.Sprintf(
				"Batch %s already exists with expiry %s (received %s). One batch number = one expiry date.",
				batchNo, existingExpiry, expiryDate,
			), 409)
			return
		}
		// Expiry matches — proceed to add qty (second delivery, same batch)
	} else if err != sql.ErrNoRows {
		http.Error(w, "DB error", 500)
		return
	}

	// Ensure batch master record exists (INSERT OR IGNORE for the matching-expiry case)
	_, err = tx.Exec(
		`INSERT OR IGNORE INTO inv_batch_master (item_id, batch_no, expiry_date) VALUES (?, ?, ?)`,
		itemID, batchNo, expiryDate,
	)
	if err != nil {
		http.Error(w, "Failed to create batch record", 500)
		return
	}

	// Get batch_id
	var batchID int64
	err = tx.QueryRow(
		`SELECT batch_id FROM inv_batch_master WHERE item_id = ? AND batch_no = ?`,
		itemID, batchNo,
	).Scan(&batchID)
	if err != nil {
		http.Error(w, "Failed to resolve batch", 500)
		return
	}

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
		headerID, itemID, itemName, batchNo, expiryDate, qty, uom, warehouseID)
	if err != nil {
		http.Error(w, "Failed to create GRN detail: "+err.Error(), 500)
		return
	}

	_, err = tx.Exec(
		`INSERT INTO inv_onhand (item_id, item_name, warehouse_id, batch_id, qty_onhand, uom)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(item_id, warehouse_id, batch_id) DO UPDATE SET
		   qty_onhand = qty_onhand + excluded.qty_onhand,
		   updated_at = datetime('now')`,
		itemID, itemName, warehouseID, batchID, qty, uom,
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
		SELECT b.batch_no, b.expiry_date, o.qty_onhand, o.uom, o.item_name
		FROM inv_onhand o
		JOIN inv_batch_master b ON o.batch_id = b.batch_id
		WHERE o.item_id = ? AND o.warehouse_id = ? AND o.qty_onhand > 0
		ORDER BY b.expiry_date ASC
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
		ItemName   string `json:"item_name"`
	}

	var batches []Batch
	total := 0
	for rows.Next() {
		var b Batch
		rows.Scan(&b.BatchNo, &b.ExpiryDate, &b.QtyOnHand, &b.UOM, &b.ItemName)
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
	selectedBatch := r.FormValue("batch_no")
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

	type alloc struct{ BatchID int64; BatchNo, ExpiryDate, UOM string; Qty int }
	var allocations []alloc

	if selectedBatch != "" {
		// Manual batch selection — allocate from specified batch only
		var b struct{ BatchID int64; BatchNo, ExpiryDate, UOM string; Qty int }
		err := tx.QueryRow(`
			SELECT b.batch_id, b.batch_no, b.expiry_date, o.qty_onhand, o.uom
			FROM inv_onhand o
			JOIN inv_batch_master b ON o.batch_id = b.batch_id
			WHERE o.item_id = ? AND o.warehouse_id = ? AND b.batch_no = ? AND o.qty_onhand > 0
		`, itemID, warehouseID, selectedBatch).Scan(&b.BatchID, &b.BatchNo, &b.ExpiryDate, &b.Qty, &b.UOM)
		if err == sql.ErrNoRows {
			http.Error(w, "Selected batch not found or has no stock", 400)
			return
		}
		if err != nil {
			http.Error(w, "Query failed", 500)
			return
		}
		if b.Qty < qtyReq {
			http.Error(w, fmt.Sprintf("Insufficient stock in batch %s: requested %d, available %d", selectedBatch, qtyReq, b.Qty), 400)
			return
		}
		allocations = append(allocations, alloc{
			BatchID: b.BatchID, BatchNo: b.BatchNo, ExpiryDate: b.ExpiryDate,
			UOM: b.UOM, Qty: qtyReq,
		})
	} else {
		// FEFO: fetch batches ordered by expiry ASC
		rows, err := tx.Query(`
			SELECT b.batch_id, b.batch_no, b.expiry_date, o.qty_onhand, o.uom
			FROM inv_onhand o
			JOIN inv_batch_master b ON o.batch_id = b.batch_id
			WHERE o.item_id = ? AND o.warehouse_id = ? AND o.qty_onhand > 0
			ORDER BY b.expiry_date ASC
		`, itemID, warehouseID)
		if err != nil {
			http.Error(w, "Query failed", 500)
			return
		}
		defer rows.Close()

		type batch struct {
			BatchID                  int64
			BatchNo, ExpiryDate, UOM string
			Qty                      int
		}
		var batches []batch
		totalAvail := 0
		for rows.Next() {
			var b batch
			rows.Scan(&b.BatchID, &b.BatchNo, &b.ExpiryDate, &b.Qty, &b.UOM)
			batches = append(batches, b)
			totalAvail += b.Qty
		}
		rows.Close()

		if totalAvail < qtyReq {
			http.Error(w, fmt.Sprintf("Insufficient stock: requested %d, available %d", qtyReq, totalAvail), 400)
			return
		}

		// FEFO allocation
		remaining := qtyReq
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
				BatchID: batches[i].BatchID, BatchNo: batches[i].BatchNo,
				ExpiryDate: batches[i].ExpiryDate, UOM: batches[i].UOM, Qty: take,
			})
		}
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
			 WHERE item_id = ? AND warehouse_id = ? AND batch_id = ?`,
			a.Qty, itemID, warehouseID, a.BatchID,
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
			<h3 style="margin-top:12px;font-size:14px;color:#666;">` + func() string {
		if selectedBatch != "" {
			return "Manual Batch Selection"
		}
		return "FEFO Allocation"
	}() + `</h3>
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
			   b.batch_no, b.expiry_date, o.qty_onhand, o.uom, o.warehouse_id, o.updated_at
		FROM inv_onhand o
		LEFT JOIN inv_item_master m ON o.item_id = m.item_id
		JOIN inv_batch_master b ON o.batch_id = b.batch_id
		`+filter+`
		ORDER BY m.brand_name, b.expiry_date
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

// POST /api/create-item
func createItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	genericName := r.FormValue("generic_name")
	brandName := r.FormValue("brand_name")
	dosageForm := r.FormValue("dosage_form")
	strength := r.FormValue("strength")
	barcode := r.FormValue("barcode")
	packSizeStr := r.FormValue("pack_size")
	uom := r.FormValue("uom")
	manufacturer := r.FormValue("manufacturer")
	country := r.FormValue("country")

	if genericName == "" || brandName == "" || dosageForm == "" || strength == "" || barcode == "" || packSizeStr == "" {
		http.Error(w, `{"error":"all fields required"}`, 400)
		return
	}
	packSize, err := strconv.Atoi(packSizeStr)
	if err != nil || packSize <= 0 {
		http.Error(w, `{"error":"invalid pack_size"}`, 400)
		return
	}
	if uom == "" {
		uom = "Pack"
	}

	// Auto-generate item_id
	var maxID sql.NullString
	db.QueryRow(`SELECT MAX(item_id) FROM inv_item_master`).Scan(&maxID)
	nextNum := 1
	if maxID.Valid && len(maxID.String) >= 7 {
		n, err := strconv.Atoi(maxID.String[4:])
		if err == nil {
			nextNum = n + 1
		}
	}
	itemID := fmt.Sprintf("ITM-%03d", nextNum)

	_, err = db.Exec(
		`INSERT INTO inv_item_master (item_id, generic_name, brand_name, dosage_form, strength, pack_size, uom, barcode, manufacturer, country)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		itemID, genericName, brandName, dosageForm, strength, packSize, uom, barcode, manufacturer, country)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "barcode already exists"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ItemMaster{
		ItemID: itemID, GenericName: genericName, BrandName: brandName,
		DosageForm: dosageForm, Strength: strength, PackSize: packSize,
		Manufacturer: manufacturer, Country: country,
		UOM: uom, Barcode: barcode,
	})
}

// POST /api/link-barcode
func linkBarcode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	itemID := r.FormValue("item_id")
	barcode := r.FormValue("barcode")
	if itemID == "" || barcode == "" {
		http.Error(w, `{"error":"item_id and barcode required"}`, 400)
		return
	}

	res, err := db.Exec(`UPDATE inv_item_master SET barcode = ? WHERE item_id = ?`, barcode, itemID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "barcode already in use"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "item not found"})
		return
	}

	// Return full item
	var it ItemMaster
	db.QueryRow(
		`SELECT item_id, generic_name, brand_name, dosage_form, strength, pack_size, uom, barcode, manufacturer, country
		 FROM inv_item_master WHERE item_id = ?`, itemID,
	).Scan(&it.ItemID, &it.GenericName, &it.BrandName, &it.DosageForm, &it.Strength, &it.PackSize, &it.UOM, &it.Barcode, &it.Manufacturer, &it.Country)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(it)
}
