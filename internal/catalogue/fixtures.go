package catalogue

import "fmt"

// Scenarios lists the fixed synthetic supplier behaviours available to staff.
func Scenarios() []string {
	return []string{"normal", "duplicates", "out_of_order", "outage", "invalid", "slow", "full", "correction"}
}

// NewManifest fixes the event stream at publication time.
func NewManifest(id, scenario string, sequence int64) (Manifest, error) {
	valid := scenario == "baseline"
	for _, candidate := range Scenarios() {
		valid = valid || scenario == candidate
	}
	if !valid {
		return Manifest{}, invalid("unknown demo scenario")
	}
	m := Manifest{ID: id, Scenario: scenario, Sequence: sequence, Total: 35, Pages: 1}
	switch scenario {
	case "baseline", "full", "slow", "outage":
		m.Total = 10000
		m.Pages = 100
	case "duplicates":
		m.Total = 70
		m.Pages = 3
	case "out_of_order":
		m.Total = 4
	case "invalid":
		m.Total = 4
	case "correction":
		m.Total = 2
	}
	return m, nil
}

// BaselineProduct generates a deterministic catalogue of 10,000 synthetic products.
func BaselineProduct(i int) Product {
	names := []string{"Canvas tote", "Ceramic mug", "Desk lamp", "Linen notebook", "Oak tray"}
	return Product{ID: fmt.Sprintf("P%05d", i), Name: fmt.Sprintf("%s %05d", names[(i-1)%len(names)], i), Price: int64(500 + i%9500), Active: true, Version: 1}
}

func normalRecord(m Manifest, i int) Record {
	p := BaselineProduct(i + 1)
	p.Version = m.Sequence*10 + 2
	if i < 10 {
		p = BaselineProduct(10001 + i)
		p.Version = m.Sequence*10 + 2
	}
	if i >= 10 && i < 30 {
		p.Price += 250
	}
	if i >= 30 {
		p.Active = false
	}
	return Record{SourceID: fmt.Sprintf("R%05d", i), Product: p}
}

// FixturePage is deterministic for a manifest and cursor, including repeated pages.
func FixturePage(m Manifest, cursor int) (Page, error) {
	if cursor < 0 || cursor >= m.Pages {
		return Page{}, fmt.Errorf("cursor outside immutable update")
	}
	p := Page{UpdateID: m.ID, Cursor: cursor, Next: cursor + 1, Total: m.Total, End: cursor == m.Pages-1, Records: []Record{}}
	switch m.Scenario {
	case "baseline", "full", "slow", "outage":
		for i := cursor*100 + 1; i <= (cursor+1)*100; i++ {
			product := BaselineProduct(i)
			if m.Scenario != "baseline" {
				product.Version = m.Sequence*10 + 2
				product.Price += 100
			}
			p.Records = append(p.Records, Record{SourceID: fmt.Sprintf("R%05d", i), Product: product})
		}
	case "duplicates":
		for i := 0; i < 35; i++ {
			r := normalRecord(m, i)
			if cursor == 2 {
				r.SourceID = fmt.Sprintf("R%05d", i+35)
			}
			p.Records = append(p.Records, r)
		}
	case "out_of_order":
		newer := BaselineProduct(1)
		newer.Version = m.Sequence*10 + 4
		newer.Price = 2400
		older := newer
		older.Version--
		older.Price = 1200
		withdrawn := BaselineProduct(2)
		withdrawn.Version = m.Sequence*10 + 4
		withdrawn.Active = false
		stale := withdrawn
		stale.Version--
		stale.Active = true
		for i, product := range []Product{newer, older, withdrawn, stale} {
			p.Records = append(p.Records, Record{SourceID: fmt.Sprintf("R%05d", i), Product: product})
		}
	case "invalid":
		for i := 0; i < 4; i++ {
			r := normalRecord(m, i+10)
			r.SourceID = fmt.Sprintf("R%05d", i)
			if i == 0 {
				r.Product.ID = ""
			}
			if i == 1 {
				r.Product.Price = -1
			}
			p.Records = append(p.Records, r)
		}
	case "correction":
		for i := 0; i < 2; i++ {
			r := normalRecord(m, i+10)
			r.SourceID = fmt.Sprintf("R%05d", i)
			p.Records = append(p.Records, r)
		}
	default:
		for i := 0; i < 35; i++ {
			p.Records = append(p.Records, normalRecord(m, i))
		}
	}
	return p, nil
}
