package main

// Singapore property tools adapted from ProProp: HDB resale comparables and
// a price-trend series over a generated demo sample, the IRAS stamp-duty
// calculators on the real rate tables, and a shortlist proposal the user
// accepts or skips. Each tool takes the smallest handle that names a thing
// (a town, a project name) and does the lookup, the normalising and the
// arithmetic in code, so no figure is left for the model to produce. The
// demo data is deterministic and generated here so the example runs
// offline; a deployment swaps demoResale and demoProjects for its own store
// and keeps the tool contracts.

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Cyprinus12138/dialogram/tool"
)

const sqftPerSqm = 10.7639

// ---------------- demo HDB resale sample ----------------

// resaleTxn is one HDB resale transaction.
type resaleTxn struct {
	Month     string // YYYY-MM
	Town      string
	Block     string
	Street    string
	FlatType  string
	Storey    string
	AreaSqm   float64
	LeaseFrom int
	Price     float64
}

func (t resaleTxn) psf() float64 { return t.Price / (t.AreaSqm * sqftPerSqm) }

// demoTown shapes one town of the generated sample. BasePSF and Growth (per
// month) drive the prices; they are not market figures.
type demoTown struct {
	Name      string
	BasePSF   float64
	Growth    float64
	Streets   []string
	LeaseFrom int
}

var demoTowns = []demoTown{
	{"ANG MO KIO", 620, 0.002, []string{"ANG MO KIO AVE 3", "ANG MO KIO AVE 10"}, 1979},
	{"BISHAN", 740, 0.003, []string{"BISHAN ST 12", "BISHAN ST 22"}, 1988},
	{"PUNGGOL", 600, 0.001, []string{"PUNGGOL FIELD", "PUNGGOL WAY"}, 2014},
	{"QUEENSTOWN", 860, 0.005, []string{"STIRLING RD", "DAWSON RD"}, 2016},
	{"TAMPINES", 580, 0.002, []string{"TAMPINES ST 21", "TAMPINES AVE 9"}, 1987},
}

var demoFlatTypes = []struct {
	Type string
	Sqm  float64
}{{"3 ROOM", 67}, {"4 ROOM", 92}, {"5 ROOM", 113}, {"EXECUTIVE", 142}}

// demoMonths is how far back the sample reaches.
const demoMonths = 24

// demoResale generates the sample for town: four transactions a month over
// the demoMonths months up to now's month, newest first. The same month
// always yields the same rows.
func demoResale(town demoTown, now time.Time) []resaleTxn {
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := make([]resaleTxn, 0, demoMonths*4)
	for m := 0; m < demoMonths; m++ {
		month := first.AddDate(0, -m, 0).Format("2006-01")
		for i := 0; i < 4; i++ {
			pick := func(salt string) float64 { return noise(town.Name, month, i, salt) }
			flat := demoFlatTypes[(i+int(pick("flat")*4))%len(demoFlatTypes)]
			storey := 1 + 3*int(pick("storey")*12)
			psf := town.BasePSF * (1 + town.Growth*float64(demoMonths-1-m)) * (0.92 + 0.16*pick("price")) * (1 + 0.004*float64(storey))
			out = append(out, resaleTxn{
				Month:     month,
				Town:      town.Name,
				Block:     fmt.Sprint(100 + int(pick("block")*600)),
				Street:    town.Streets[int(pick("street")*float64(len(town.Streets)))],
				FlatType:  flat.Type,
				Storey:    fmt.Sprintf("%02d TO %02d", storey, storey+2),
				AreaSqm:   flat.Sqm,
				LeaseFrom: town.LeaseFrom,
				Price:     math.Round(psf*flat.Sqm*sqftPerSqm/1000) * 1000,
			})
		}
	}
	return out
}

// noise is a deterministic value in [0, 1) for one field of one generated row.
func noise(parts ...any) float64 {
	h := fnv.New64a()
	fmt.Fprintln(h, parts...)
	return float64(h.Sum64()%1_000_000) / 1_000_000
}

func demoResaleSource(coverage string) tool.Source {
	return tool.Source{
		Dataset:     "demo HDB resale sample (generated, not market data)",
		RetrievedAt: time.Now().UTC().Format(time.RFC3339),
		Coverage:    coverage,
	}
}

// ---------------- normalising inputs ----------------

// findTown resolves a town as people write it ("ang mo kio", " Bishan ").
func findTown(s string) (demoTown, bool) {
	name := strings.Join(strings.Fields(strings.ToUpper(s)), " ")
	for _, t := range demoTowns {
		if t.Name == name {
			return t, true
		}
	}
	return demoTown{}, false
}

func unknownTown(s string) string {
	names := make([]string, len(demoTowns))
	for i, t := range demoTowns {
		names[i] = t.Name
	}
	return fmt.Sprintf("no data for town %q; the demo sample covers %s", s, strings.Join(names, ", "))
}

// normFlatType maps the ways people write an HDB flat type onto the stored
// form: "4-room", "4 rm", "4RM" and "four room" all become "4 ROOM". An
// empty input stays empty (all flat types); anything unrecognised is
// returned upper-cased for the caller to reject.
func normFlatType(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", " ", "_", " ").Replace(s))
	s = strings.NewReplacer("TWO", "2", "THREE", "3", "FOUR", "4", "FIVE", "5").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	if s == "EXEC" {
		return "EXECUTIVE"
	}
	if len(s) >= 1 && s[0] >= '1' && s[0] <= '5' {
		if rest := strings.TrimSpace(s[1:]); rest == "" || rest == "RM" || rest == "ROOM" {
			return s[:1] + " ROOM"
		}
	}
	return s
}

func knownFlatType(s string) bool {
	for _, f := range demoFlatTypes {
		if f.Type == s {
			return true
		}
	}
	return false
}

// clampInt treats a missing or non-positive value as def and caps it at max:
// the model fills optional numbers with 0 as readily as it omits them.
func clampInt(v, def, max int) int {
	if v <= 0 {
		return def
	}
	return min(v, max)
}

// resaleQuery is the input both market tools share.
type resaleQuery struct {
	Town     string `json:"town"`
	FlatType string `json:"flat_type"`
}

// rows resolves the query and returns the town's transactions in the last
// months months, newest first, or a soft failure for the model.
func (q resaleQuery) rows(months int) (demoTown, string, []resaleTxn, *tool.Envelope) {
	town, ok := findTown(q.Town)
	if !ok {
		return demoTown{}, "", nil, tool.SoftEnvelope(tool.ErrorCodeNotFound, unknownTown(q.Town))
	}
	flat := normFlatType(q.FlatType)
	if flat != "" && !knownFlatType(flat) {
		return demoTown{}, "", nil, tool.SoftEnvelope(tool.ErrorCodeInvalidInput,
			fmt.Sprintf("flat_type %q is not one of 3 ROOM, 4 ROOM, 5 ROOM, EXECUTIVE", q.FlatType))
	}
	now := time.Now()
	since := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1-months, 0).Format("2006-01")
	var out []resaleTxn
	for _, r := range demoResale(town, now) {
		if r.Month >= since && (flat == "" || r.FlatType == flat) {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return demoTown{}, "", nil, tool.SoftEnvelope(tool.ErrorCodeNotFound,
			fmt.Sprintf("no %s transactions in %s in the last %d months", orAll(flat), town.Name, months))
	}
	return town, flat, out, nil
}

func orAll(flat string) string {
	if flat == "" {
		return "HDB"
	}
	return flat
}

var flatTypeEnum = tool.Enum("HDB flat type; omit for all types", "3 ROOM", "4 ROOM", "5 ROOM", "EXECUTIVE")

// ---------------- lookup_hdb_resale_comps ----------------

func resaleCompsTool() *tool.Tool {
	return &tool.Tool{
		Name:  "lookup_hdb_resale_comps",
		Label: "HDB resale comparables",
		Description: "HDB resale transactions in a town, optionally for one flat type, over a look-back window. " +
			"Returns summary statistics (count, median price, median and quartile price per square foot), the three most recent " +
			"transactions, and an artifact ref to the full table. Show the table with render_from_ref; do not retype the rows.",
		Schema: tool.Obj(map[string]any{
			"town":        tool.Str("HDB town, e.g. BISHAN or Ang Mo Kio"),
			"flat_type":   flatTypeEnum,
			"months_back": tool.Int("Look-back window in months (default 12, max 24)"),
			"limit":       tool.Int("Rows kept in the table artifact, newest first (default 20, max 100)"),
		}, "town"),
		Annotations: tool.Annotations{ReadOnly: true, Idempotent: true},
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			var in struct {
				resaleQuery
				MonthsBack int `json:"months_back"`
				Limit      int `json:"limit"`
			}
			_ = json.Unmarshal(input, &in)
			months := clampInt(in.MonthsBack, 12, demoMonths)
			town, flat, rows, soft := in.rows(months)
			if soft != nil {
				return soft, nil
			}
			prices := make([]float64, len(rows))
			psfs := make([]float64, len(rows))
			for i, r := range rows {
				prices[i], psfs[i] = r.Price, r.psf()
			}
			keep := rows[:min(len(rows), clampInt(in.Limit, 20, 100))]
			table := make([][]any, 0, len(keep))
			for _, r := range keep {
				table = append(table, []any{r.Month, r.Block + " " + r.Street, r.FlatType, r.Storey, r.AreaSqm, r.LeaseFrom, r.Price, round2(r.psf())})
			}
			sample := make([]map[string]any, 0, 3)
			for _, r := range rows[:min(len(rows), 3)] {
				sample = append(sample, map[string]any{"month": r.Month, "address": r.Block + " " + r.Street, "flat_type": r.FlatType,
					"storey": r.Storey, "resale_price": r.Price, "psf": round2(r.psf())})
			}
			scope := fmt.Sprintf("%s %s, last %d months", town.Name, orAll(flat), months)
			return &tool.Envelope{
				ModelObservation: map[string]any{
					"town": town.Name, "flat_type": orAll(flat), "months_back": months,
					"summary": map[string]any{
						"count":        len(rows),
						"median_price": round2(median(prices)),
						"median_psf":   round2(median(psfs)),
						"p25_psf":      round2(percentile(psfs, 0.25)),
						"p75_psf":      round2(percentile(psfs, 0.75)),
					},
					"sample": sample,
					"source": demoResaleSource(scope),
				},
				RuntimeState: &tool.TableRecord{
					Columns: []tool.TableColumn{{Key: "month", Label: "Month"}, {Key: "address", Label: "Address"}, {Key: "flat_type", Label: "Flat type"},
						{Key: "storey", Label: "Storey"}, {Key: "area_sqm", Label: "Area (sqm)", Type: "number"}, {Key: "lease_from", Label: "Lease from", Type: "number"},
						{Key: "price", Label: "Price (S$)", Type: "currency"}, {Key: "psf", Label: "S$ psf", Type: "number"}},
					Rows: table,
				},
				Artifacts: []tool.ArtifactRef{{ID: tool.ToolUseIDFromContext(ctx), Type: tool.ShapeTableRecord,
					Description: fmt.Sprintf("%d resale transactions, %s", len(table), scope)}},
			}, nil
		},
	}
}

// ---------------- psf_trend_series ----------------

func psfTrendTool() *tool.Tool {
	return &tool.Tool{
		Name:  "psf_trend_series",
		Label: "Price trend",
		Description: "Monthly median price per square foot of HDB resale in a town, optionally for one flat type, with a linear trend " +
			"(percent per year and r²). The user sees a line chart; you get the points.",
		Schema: tool.Obj(map[string]any{
			"town":      tool.Str("HDB town, e.g. TAMPINES"),
			"flat_type": flatTypeEnum,
			"months":    tool.Int("Window length in months (default 24, max 24)"),
		}, "town"),
		Annotations: tool.Annotations{ReadOnly: true, Idempotent: true},
		Fn: func(ctx context.Context, input json.RawMessage) (any, error) {
			var in struct {
				resaleQuery
				Months int `json:"months"`
			}
			_ = json.Unmarshal(input, &in)
			months := clampInt(in.Months, demoMonths, demoMonths)
			town, flat, rows, soft := in.rows(months)
			if soft != nil {
				return soft, nil
			}
			byMonth := map[string][]float64{}
			for _, r := range rows {
				byMonth[r.Month] = append(byMonth[r.Month], r.psf())
			}
			keys := make([]string, 0, len(byMonth))
			for m := range byMonth {
				keys = append(keys, m)
			}
			sort.Strings(keys)
			points := make([]map[string]any, 0, len(keys))
			chart := make([]map[string]any, 0, len(keys))
			var xs, ys []float64
			for _, m := range keys {
				med := round2(median(byMonth[m]))
				points = append(points, map[string]any{"month": m, "median_psf": med, "txn_count": len(byMonth[m])})
				chart = append(chart, map[string]any{"label": m, "value": med})
				xs = append(xs, float64(monthIndex(m)))
				ys = append(ys, med)
			}
			slope, r2 := linReg(xs, ys)
			perYear := 0.0
			if base := median(ys); base > 0 {
				perYear = slope * 12 / base * 100
			}
			title := fmt.Sprintf("Median S$ psf, %s %s", town.Name, orAll(flat))
			return &tool.Envelope{
				ModelObservation: map[string]any{
					"town": town.Name, "flat_type": orAll(flat), "points": points,
					"trend":  map[string]any{"slope_pct_per_year": round2(perYear), "r_squared": round2(r2)},
					"source": demoResaleSource(fmt.Sprintf("%s %s, last %d months", town.Name, orAll(flat), months)),
				},
				UIPayload: &tool.UIPayload{Component: tool.ComponentLineChart, Data: map[string]any{
					"type": tool.ComponentLineChart, "title": title, "data": chart, "y_label": "S$ psf",
				}},
			}, nil
		},
	}
}

// monthIndex counts months from year zero, so gaps in a series stay gaps.
func monthIndex(m string) int {
	t, err := time.Parse("2006-01", m)
	if err != nil {
		return 0
	}
	return t.Year()*12 + int(t.Month())
}

// ---------------- calc_bsd / calc_absd ----------------

// The IRAS rate tables, with the dates they apply from. A purchase price
// above maxPriceSGD is a placeholder or a typo, not a property.
const (
	bsdRatesFrom     = "2023-02-15"
	absdRatesFrom    = "2023-04-27"
	irasStampDutyURL = "https://www.iras.gov.sg/taxes/stamp-duty/for-property"
	maxPriceSGD      = 1e12
)

type dutyBand struct {
	Upper float64 // the band ends here; +Inf for the last
	Rate  float64
}

var (
	bsdResidential    = []dutyBand{{180_000, 0.01}, {360_000, 0.02}, {1_000_000, 0.03}, {1_500_000, 0.04}, {3_000_000, 0.05}, {math.Inf(1), 0.06}}
	bsdNonResidential = []dutyBand{{180_000, 0.01}, {360_000, 0.02}, {1_000_000, 0.03}, {1_500_000, 0.04}, {math.Inf(1), 0.05}}
)

// absdRates holds the rate by residency for a buyer's first, second, and
// third or later residential property.
var absdRates = map[string][3]float64{
	"SC":        {0.00, 0.20, 0.30},
	"PR":        {0.05, 0.30, 0.35},
	"FOREIGNER": {0.60, 0.60, 0.60},
	"ENTITY":    {0.65, 0.65, 0.65},
}

func badPrice(v float64) bool { return !(v > 0) || v > maxPriceSGD || math.IsInf(v, 0) }

// bsd applies the bands to price and returns the duty with one line per band
// the price reaches.
func bsd(price float64, bands []dutyBand) (float64, []map[string]any) {
	total, lower := 0.0, 0.0
	var lines []map[string]any
	for _, b := range bands {
		if price <= lower {
			break
		}
		amount := (math.Min(price, b.Upper) - lower) * b.Rate
		total += amount
		band := fmt.Sprintf("%.0f-%.0f", lower, b.Upper)
		if math.IsInf(b.Upper, 1) {
			band = fmt.Sprintf("%.0f+", lower)
		}
		lines = append(lines, map[string]any{"band": band, "rate_pct": round2(b.Rate * 100), "amount_sgd": round2(amount)})
		lower = b.Upper
	}
	return round2(total), lines
}

func irasSource(dataset, from string) tool.Source {
	return tool.Source{Dataset: dataset, Publisher: "IRAS", URL: irasStampDutyURL,
		RetrievedAt: time.Now().UTC().Format(time.RFC3339), Coverage: "rates in force from " + from}
}

func bsdTool() *tool.Tool {
	return &tool.Tool{
		Name:        "calc_bsd",
		Label:       "Buyer's Stamp Duty",
		Description: "Buyer's Stamp Duty (BSD) on a Singapore property purchase at the IRAS rates. Returns the duty and a band-by-band breakdown.",
		Schema: tool.Obj(map[string]any{
			"purchase_price_sgd": tool.Num("Purchase price, or market value if higher, in SGD"),
			"property_type":      tool.Enum("Property category (default RESIDENTIAL)", "RESIDENTIAL", "NON_RESIDENTIAL"),
		}, "purchase_price_sgd"),
		Annotations: tool.Annotations{ReadOnly: true, Idempotent: true},
		Fn: func(_ context.Context, input json.RawMessage) (any, error) {
			var in struct {
				Price        float64 `json:"purchase_price_sgd"`
				PropertyType string  `json:"property_type"`
			}
			_ = json.Unmarshal(input, &in)
			if badPrice(in.Price) {
				return tool.Soft(tool.ErrorCodeInvalidInput, "purchase_price_sgd must be a positive amount in SGD"), nil
			}
			bands, kind := bsdResidential, "RESIDENTIAL"
			if strings.EqualFold(strings.TrimSpace(in.PropertyType), "NON_RESIDENTIAL") {
				bands, kind = bsdNonResidential, "NON_RESIDENTIAL"
			}
			duty, lines := bsd(in.Price, bands)
			return map[string]any{
				"bsd_sgd": duty, "purchase_price_sgd": in.Price, "property_type": kind, "breakdown": lines,
				"rates_from": bsdRatesFrom, "source": irasSource("IRAS Buyer's Stamp Duty rates", bsdRatesFrom),
			}, nil
		},
	}
}

// normResidency maps how people describe a buyer onto the IRAS categories.
func normResidency(s string) (string, bool) {
	s = strings.Join(strings.Fields(strings.ToUpper(strings.NewReplacer("-", " ", "_", " ", ".", "").Replace(s))), " ")
	switch s {
	case "SC", "CITIZEN", "SINGAPORE CITIZEN", "SINGAPOREAN":
		return "SC", true
	case "PR", "SPR", "PERMANENT RESIDENT", "SINGAPORE PERMANENT RESIDENT":
		return "PR", true
	case "FOREIGNER", "FOREIGN", "FOREIGN BUYER":
		return "FOREIGNER", true
	case "ENTITY", "COMPANY", "CORPORATE":
		return "ENTITY", true
	}
	return "", false
}

func absdTool() *tool.Tool {
	return &tool.Tool{
		Name:  "calc_absd",
		Label: "Additional Buyer's Stamp Duty",
		Description: "Additional Buyer's Stamp Duty (ABSD) on a Singapore residential purchase at the IRAS rates. " +
			"For a joint purchase the highest rate among the buyers applies to the whole price. Remissions and refunds are not applied.",
		Schema: tool.Obj(map[string]any{
			"buyer_profile": tool.Arr(tool.Obj(map[string]any{
				"residency":                  tool.Enum("Buyer's residency", "SC", "PR", "FOREIGNER", "ENTITY"),
				"existing_residential_count": tool.Int("Residential properties this buyer already owns"),
			}, "residency", "existing_residential_count"), "One entry per buyer in the purchase"),
			"purchase_price_sgd": tool.Num("Purchase price, or market value if higher, in SGD"),
		}, "buyer_profile", "purchase_price_sgd"),
		Annotations: tool.Annotations{ReadOnly: true, Idempotent: true},
		Fn: func(_ context.Context, input json.RawMessage) (any, error) {
			var in struct {
				Buyers []struct {
					Residency string `json:"residency"`
					Existing  int    `json:"existing_residential_count"`
				} `json:"buyer_profile"`
				Price float64 `json:"purchase_price_sgd"`
			}
			_ = json.Unmarshal(input, &in)
			if badPrice(in.Price) || len(in.Buyers) == 0 {
				return tool.Soft(tool.ErrorCodeInvalidInput, "buyer_profile needs at least one buyer and purchase_price_sgd a positive amount in SGD"), nil
			}
			rate := 0.0
			buyers := make([]map[string]any, 0, len(in.Buyers))
			for _, b := range in.Buyers {
				res, ok := normResidency(b.Residency)
				if !ok {
					return tool.Soft(tool.ErrorCodeInvalidInput, fmt.Sprintf("residency %q is not one of SC, PR, FOREIGNER, ENTITY", b.Residency)), nil
				}
				if b.Existing < 0 {
					return tool.Soft(tool.ErrorCodeInvalidInput, "existing_residential_count cannot be negative"), nil
				}
				r := absdRates[res][min(b.Existing, 2)]
				rate = math.Max(rate, r)
				buyers = append(buyers, map[string]any{"residency": res, "existing_residential_count": b.Existing, "rate_pct": round2(r * 100)})
			}
			return map[string]any{
				"absd_sgd": round2(rate * in.Price), "applied_rate_pct": round2(rate * 100), "purchase_price_sgd": in.Price, "buyers": buyers,
				"rule":       "The highest rate among the buyers applies to the whole price.",
				"rates_from": absdRatesFrom, "source": irasSource("IRAS Additional Buyer's Stamp Duty rates", absdRatesFrom),
			}, nil
		},
	}
}

// ---------------- propose_shortlist ----------------

const shortlistToolName = "propose_shortlist"

// project is a row of the demo project table, the canonical record a name
// resolves to. The projects are fictional.
type project struct {
	Name       string `json:"name"`
	District   string `json:"district"`
	Segment    string `json:"market_segment"`
	Tenure     string `json:"tenure"`
	TOPYear    int    `json:"top_year"`
	Street     string `json:"street"`
	NearestMRT string `json:"nearest_mrt"`
	WalkMin    int    `json:"walk_min"`
}

var demoProjects = []project{
	{"KESTREL GARDENS", "20", "OCR", "99-year leasehold from 2018", 2022, "BISHAN ST 13", "Bishan (NS17/CC15)", 6},
	{"MAPLEWOOD TERRACE", "18", "OCR", "99-year leasehold from 2020", 2024, "TAMPINES AVE 10", "Tampines (EW2/DT32)", 8},
	{"CORAL STIRLING", "03", "RCR", "99-year leasehold from 2019", 2023, "STIRLING RD", "Queenstown (EW19)", 5},
	{"OSPREY POINT", "19", "OCR", "99-year leasehold from 2021", 2025, "PUNGGOL WALK", "Punggol (NE17)", 7},
}

// findProject resolves a project by its name as written, case and spacing
// aside. There is no fuzzy matching: a near miss is not a project.
func findProject(s string) (project, bool) {
	name := strings.Join(strings.Fields(strings.ToUpper(s)), " ")
	for _, p := range demoProjects {
		if p.Name == name {
			return p, true
		}
	}
	return project{}, false
}

// shortlistTool is an application pause tool (Config.PauseTools): the loop
// never runs it. The model names a project; the user sees the project's
// facts from the table (shortlistCard) and accepts or skips, and
// /chat/respond answers the call with shortlistAnswer. The model passes no
// description of its own: whatever the card says about the project comes
// from the table, so the model has nothing to get wrong on it.
func shortlistTool() *tool.Tool {
	return &tool.Tool{
		Name:  shortlistToolName,
		Label: "Propose for shortlist",
		Description: "Propose adding a project to the user's shortlist. The conversation pauses and the user accepts or skips it in the chat. " +
			"Pass only the project name: the server shows the user the project's district, tenure, completion year and nearest MRT " +
			"from its own records. Propose at most one project per turn, and only one the user has shown interest in.",
		Schema: tool.Obj(map[string]any{
			"project_name": tool.Str("The project's name, e.g. KESTREL GARDENS"),
		}, "project_name"),
		Fn: func(context.Context, json.RawMessage) (any, error) { return nil, tool.ErrHITLShortCircuit },
	}
}

// shortlistEntry is a project on a conversation's shortlist, resolved when
// the user accepted it.
type shortlistEntry struct {
	project
	AddedAt string `json:"added_at"`
}

// shortlistCard is the text the user reads on a propose_shortlist pause:
// the project's facts from the table, or that the table has no such
// project.
func shortlistCard(input json.RawMessage) string {
	var in struct {
		ProjectName string `json:"project_name"`
	}
	_ = json.Unmarshal(input, &in)
	p, ok := findProject(in.ProjectName)
	if !ok {
		return fmt.Sprintf("Add %q to your shortlist? There is no project by that name in the project table.", in.ProjectName)
	}
	return fmt.Sprintf("Add %s to your shortlist? District %s (%s), %s, completed %d, %d min walk to %s.",
		p.Name, p.District, p.Segment, p.Tenure, p.TOPYear, p.WalkMin, p.NearestMRT)
}

// shortlistAnswer is the tool result for an answered propose_shortlist
// pause, and the entry to store when the user accepted a project the table
// knows.
func shortlistAnswer(input json.RawMessage, accepted bool) (*shortlistEntry, json.RawMessage) {
	var in struct {
		ProjectName string `json:"project_name"`
	}
	_ = json.Unmarshal(input, &in)
	if !accepted {
		return nil, jsonBody(map[string]any{"added": false, "skipped": true,
			"message": "The user skipped this project. Do not propose it again in this conversation."})
	}
	p, ok := findProject(in.ProjectName)
	if !ok {
		return nil, jsonBody(tool.Soft(tool.ErrorCodeNotFound, fmt.Sprintf("no project named %q in the project table, so nothing was added", in.ProjectName)))
	}
	e := &shortlistEntry{project: p, AddedAt: time.Now().UTC().Format(time.RFC3339)}
	return e, jsonBody(map[string]any{"added": true, "project": p,
		"source": tool.Source{Dataset: "demo project table (fictional projects)"}})
}

// ---------------- helpers ----------------

func jsonBody(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

func median(vs []float64) float64 { return percentile(vs, 0.5) }

// percentile interpolates linearly between the closest ranks.
func percentile(vs []float64, p float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	idx := p * float64(len(s)-1)
	lo := int(idx)
	if lo+1 >= len(s) {
		return s[lo]
	}
	return s[lo] + (s[lo+1]-s[lo])*(idx-float64(lo))
}

// linReg fits y = a + slope·x by least squares and returns the slope and r².
func linReg(xs, ys []float64) (slope, r2 float64) {
	n := float64(len(xs))
	if n < 2 {
		return 0, 0
	}
	var sx, sy, sxx, syy, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		syy += ys[i] * ys[i]
		sxy += xs[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, 0
	}
	slope = (n*sxy - sx*sy) / den
	if d := math.Sqrt(den * (n*syy - sy*sy)); d > 0 {
		r := (n*sxy - sx*sy) / d
		r2 = r * r
	}
	return slope, r2
}
