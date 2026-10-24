package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cyprinus12138/dialogram"
	"github.com/Cyprinus12138/dialogram/agent"
	"github.com/Cyprinus12138/dialogram/streambuf"
	"github.com/Cyprinus12138/dialogram/tool"
)

// run calls t's Fn as the loop would, under the tool_use_id id.
func run(t *testing.T, tl *tool.Tool, id, input string) any {
	t.Helper()
	out, err := tl.Fn(tool.WithCallContext(context.Background(), id, nil), json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// object round-trips v through JSON, as the model and the store see it.
func object(t *testing.T, v any) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(jsonBody(v), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBSDBands(t *testing.T) {
	cases := []struct {
		price float64
		kind  string
		want  float64
		bands int
	}{
		{180_000, "", 1_800, 1},
		{360_000, "", 5_400, 2},
		{1_000_000, "", 24_600, 3},
		{1_500_000, "", 44_600, 4},
		{3_000_000, "", 119_600, 5},
		{4_000_000, "", 179_600, 6},
		{2_000_000, "NON_RESIDENTIAL", 69_600, 5},
	}
	for _, c := range cases {
		in, _ := json.Marshal(map[string]any{"purchase_price_sgd": c.price, "property_type": c.kind})
		got := object(t, run(t, bsdTool(), "", string(in)))
		if got["bsd_sgd"] != c.want || len(got["breakdown"].([]any)) != c.bands || got["rates_from"] != bsdRatesFrom {
			t.Errorf("BSD on %.0f %s = %v over %d bands, want %.0f over %d", c.price, c.kind, got["bsd_sgd"], len(got["breakdown"].([]any)), c.want, c.bands)
		}
	}
	for _, in := range []string{`{}`, `{"purchase_price_sgd":0}`, `{"purchase_price_sgd":-5}`, `{"purchase_price_sgd":1e13}`} {
		if got := object(t, run(t, bsdTool(), "", in)); got["error_code"] != tool.ErrorCodeInvalidInput {
			t.Errorf("%s should be refused, got %v", in, got)
		}
	}
}

func TestABSDRates(t *testing.T) {
	cases := []struct {
		buyers string
		rate   float64
	}{
		{`[{"residency":"SC","existing_residential_count":0}]`, 0},
		{`[{"residency":"Singapore citizen","existing_residential_count":1}]`, 20},
		{`[{"residency":"sc","existing_residential_count":4}]`, 30},
		{`[{"residency":"permanent resident","existing_residential_count":0}]`, 5},
		{`[{"residency":"SC","existing_residential_count":0},{"residency":"PR","existing_residential_count":1}]`, 30},
		{`[{"residency":"PR","existing_residential_count":1},{"residency":"SC","existing_residential_count":0}]`, 30},
		{`[{"residency":"foreigner","existing_residential_count":0}]`, 60},
		{`[{"residency":"company","existing_residential_count":0}]`, 65},
	}
	for _, c := range cases {
		got := object(t, run(t, absdTool(), "", `{"purchase_price_sgd":1200000,"buyer_profile":`+c.buyers+`}`))
		if got["applied_rate_pct"] != c.rate || got["absd_sgd"] != 1_200_000*c.rate/100 {
			t.Errorf("%s: rate %v%%, duty %v; want %v%%", c.buyers, got["applied_rate_pct"], got["absd_sgd"], c.rate)
		}
	}
	for _, in := range []string{
		`{"purchase_price_sgd":1200000,"buyer_profile":[]}`,
		`{"purchase_price_sgd":1200000,"buyer_profile":[{"residency":"tourist","existing_residential_count":0}]}`,
		`{"purchase_price_sgd":1200000,"buyer_profile":[{"residency":"SC","existing_residential_count":-1}]}`,
		`{"purchase_price_sgd":0,"buyer_profile":[{"residency":"SC","existing_residential_count":0}]}`,
	} {
		if got := object(t, run(t, absdTool(), "", in)); got["error_code"] != tool.ErrorCodeInvalidInput {
			t.Errorf("%s should be refused, got %v", in, got)
		}
	}
}

func TestInformalInputsNormalise(t *testing.T) {
	for in, want := range map[string]string{"4-room": "4 ROOM", "4 rm": "4 ROOM", "4RM": "4 ROOM", "four room": "4 ROOM",
		"3 Room": "3 ROOM", "exec": "EXECUTIVE", "Executive": "EXECUTIVE", "": "", "penthouse": "PENTHOUSE"} {
		if got := normFlatType(in); got != want {
			t.Errorf("normFlatType(%q) = %q, want %q", in, got, want)
		}
	}
	if town, ok := findTown("  ang mo  kio "); !ok || town.Name != "ANG MO KIO" {
		t.Errorf("findTown = %v %v", town.Name, ok)
	}
	if _, ok := findProject("kestrel  gardens"); !ok {
		t.Error("a project name resolves case and spacing aside")
	}
	if _, ok := findProject("kestrel garden"); ok {
		t.Error("a near miss must not resolve to a project")
	}
}

func TestResaleComps(t *testing.T) {
	env, ok := run(t, resaleCompsTool(), "dg_comps", `{"town":"bishan","flat_type":"4-room","months_back":0,"limit":5}`).(*tool.Envelope)
	if !ok {
		t.Fatal("comps should return an envelope")
	}
	obs := object(t, env.ModelObservation)
	sum := obs["summary"].(map[string]any)
	if obs["town"] != "BISHAN" || obs["flat_type"] != "4 ROOM" || obs["months_back"] != float64(12) {
		t.Errorf("inputs not normalised: %v", obs)
	}
	count := sum["count"].(float64)
	if count < 1 || sum["p25_psf"].(float64) > sum["median_psf"].(float64) || sum["median_psf"].(float64) > sum["p75_psf"].(float64) {
		t.Errorf("summary = %v", sum)
	}
	sample := obs["sample"].([]any)
	if len(sample) != min(3, int(count)) || sample[0].(map[string]any)["flat_type"] != "4 ROOM" {
		t.Errorf("sample = %v", sample)
	}
	rows := env.RuntimeState.(*tool.TableRecord).Rows
	if len(rows) != min(5, int(count)) || len(env.Artifacts) != 1 || env.Artifacts[0].ID != "dg_comps" {
		t.Errorf("table of %d rows, artifacts %v", len(rows), env.Artifacts)
	}
	seen := tool.ProjectModelObservation(jsonBody(env))
	if !strings.Contains(string(seen), `"artifacts":[{"id":"dg_comps"`) || strings.Contains(string(seen), "runtime_state") {
		t.Errorf("the model should read the handle and not the rows: %s", seen)
	}

	for in, code := range map[string]string{
		`{"town":"atlantis"}`:                       tool.ErrorCodeNotFound,
		`{"town":"bishan","flat_type":"penthouse"}`: tool.ErrorCodeInvalidInput,
	} {
		soft := object(t, run(t, resaleCompsTool(), "dg_x", in).(*tool.Envelope).ModelObservation)
		if soft["error_code"] != code {
			t.Errorf("%s: %v, want %s", in, soft, code)
		}
	}
	if soft := object(t, run(t, resaleCompsTool(), "dg_x", `{"town":"atlantis"}`).(*tool.Envelope).ModelObservation); !strings.Contains(soft["error_message"].(string), "QUEENSTOWN") {
		t.Errorf("a miss should list the towns the model can use: %v", soft)
	}
}

func TestPSFTrend(t *testing.T) {
	env := run(t, psfTrendTool(), "dg_trend", `{"town":"Queenstown"}`).(*tool.Envelope)
	obs := object(t, env.ModelObservation)
	points := obs["points"].([]any)
	if len(points) != demoMonths {
		t.Fatalf("one point per month: got %d", len(points))
	}
	for i := 1; i < len(points); i++ {
		if points[i-1].(map[string]any)["month"].(string) >= points[i].(map[string]any)["month"].(string) {
			t.Fatalf("points out of order at %d: %v", i, points)
		}
	}
	trend := obs["trend"].(map[string]any)
	if slope, r2 := trend["slope_pct_per_year"].(float64), trend["r_squared"].(float64); slope <= 0 || r2 < 0 || r2 > 1 {
		t.Errorf("the demo town rises: trend %v", trend)
	}
	chart := object(t, env.UIPayload.Data)
	if env.UIPayload.Component != tool.ComponentLineChart || chart["type"] != tool.ComponentLineChart || len(chart["data"].([]any)) != len(points) {
		t.Errorf("the user gets a line chart of the same points: %v", chart)
	}
}

// count is how many requests the fake model has received.
func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

// resultFor returns the tool message for the call to name in msgs. The loop
// gives calls neutral ids, so the lookup goes through the assistant
// message's tool_calls.
func resultFor(msgs []map[string]any, name string) string {
	ids := map[any]string{}
	out := ""
	for _, m := range msgs {
		calls, _ := m["tool_calls"].([]any)
		for _, c := range calls {
			cm := c.(map[string]any)
			ids[cm["id"]] = cm["function"].(map[string]any)["name"].(string)
		}
		if m["role"] == "tool" && ids[m["tool_call_id"]] == name {
			out, _ = m["content"].(string)
		}
	}
	return out
}

func storedConversation(t *testing.T, base, conv string) conversation {
	t.Helper()
	resp, err := http.Get(base + "/conversations/" + conv)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var c conversation
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func storedShortlist(t *testing.T, base, conv string) []shortlistEntry {
	t.Helper()
	return storedConversation(t, base, conv).Shortlist
}

func TestComparablesHandleReachesTheModel(t *testing.T) {
	u := &upstream{streams: []string{
		chunk(callDelta(0, "call_c", "lookup_hdb_resale_comps", `{"town":"tampines","flat_type":"5 room"}`)) + finish("tool_calls"),
		chunk(`{"role":"assistant","content":"Here they are."}`) + finish("stop"),
	}}
	base := testServer(t, u)
	frames := send(t, base, "m1", map[string]any{"message": "5-room resale prices in Tampines?"})
	end, conv := ended(t, frames)
	if end.event != "done" {
		t.Fatalf("the turn completes: %s %v", end.event, end.data)
	}
	for _, f := range frames {
		if out, _ := f.data["output"].(string); f.event == "tool_result" && (strings.Contains(out, "runtime_state") || !strings.Contains(out, `"artifacts"`)) {
			t.Errorf("the tool_result frame keeps the rows on the server: %s", out)
		}
	}
	for _, turn := range storedTurns(t, base, conv) {
		for _, b := range turn.Blocks {
			if strings.Contains(string(b.ToolResult), "runtime_state") {
				t.Errorf("the conversation endpoint keeps the rows on the server: %s", b.ToolResult)
			}
		}
	}
	got := resultFor(u.messages(t, 1), "lookup_hdb_resale_comps")
	if !strings.Contains(got, `"median_psf"`) || !strings.Contains(got, `"artifacts":[{"id":"dg_`) || strings.Contains(got, "runtime_state") {
		t.Errorf("the model reads the summary and the handle, not the rows: %s", got)
	}
}

func TestShortlistAcceptResolvesTheProject(t *testing.T) {
	u := &upstream{streams: []string{
		// A model may still send a description of its own; nothing shows it.
		chunk(callDelta(0, "call_s", shortlistToolName, `{"project_name":"kestrel gardens","reason":"A non-mature estate."}`)) + finish("tool_calls"),
		chunk(`{"role":"assistant","content":"Added Kestrel Gardens."}`) + finish("stop"),
	}}
	base := testServer(t, u)
	end, conv := ended(t, send(t, base, "m1", map[string]any{"message": "Kestrel Gardens looks good."}))
	if end.event != "waiting_for_user" || end.data["kind"] != agent.PauseKindCustom || end.data["name"] != shortlistToolName {
		t.Fatalf("the proposal waits for the user: %s %v", end.event, end.data)
	}
	card := end.data["message"].(string)
	if card != shortlistCard(json.RawMessage(`{"project_name":"kestrel gardens"}`)) || !strings.Contains(card, "District 20") || strings.Contains(card, "non-mature") {
		t.Errorf("the card shows the table's facts only: %q", card)
	}
	if c := storedConversation(t, base, conv); c.Pending == nil || c.Pending.Prompt != card {
		t.Errorf("a reload shows the same card: %+v", c.Pending)
	}
	if u.count() != 1 {
		t.Fatalf("nothing runs before the answer: %d model requests", u.count())
	}
	id := end.data["tool_use_id"].(string)

	missing := respond(t, base, map[string]any{"conversation_id": conv, "tool_use_id": id})
	missing.Body.Close()
	if missing.StatusCode != http.StatusBadRequest {
		t.Errorf("an answer without selected is refused, got %d", missing.StatusCode)
	}
	resp := respond(t, base, map[string]any{"conversation_id": conv, "tool_use_id": id, "message_id": "m2", "selected": []string{"accept"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("respond: %d", resp.StatusCode)
	}
	if end, _ := ended(t, tail(t, base, "m2")); end.event != "done" {
		t.Fatalf("the resumed turn completes: %s %v", end.event, end.data)
	}

	list := storedShortlist(t, base, conv)
	if len(list) != 1 || list[0].Name != "KESTREL GARDENS" || list[0].District != "20" || list[0].TOPYear != 2022 {
		t.Fatalf("the server keeps the resolved record: %+v", list)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(resultFor(u.messages(t, 1), shortlistToolName)), &body)
	if p, _ := body["project"].(map[string]any); body["added"] != true || p["district"] != "20" || p["nearest_mrt"] != "Bishan (NS17/CC15)" {
		t.Errorf("the model reads what was added: %v", body)
	}
}

func TestShortlistSkipAndUnknownAddNothing(t *testing.T) {
	for _, c := range []struct {
		name, project, answer, want string
	}{
		{"skip", "kestrel gardens", "skip", `"skipped":true`},
		{"unknown", "kestrel garden", "accept", tool.ErrorCodeNotFound},
	} {
		u := &upstream{streams: []string{
			chunk(callDelta(0, "call_s", shortlistToolName, `{"project_name":"`+c.project+`","reason":"Worth a look."}`)) + finish("tool_calls"),
			chunk(`{"role":"assistant","content":"OK."}`) + finish("stop"),
		}}
		base := testServer(t, u)
		end, conv := ended(t, send(t, base, "m1", map[string]any{"message": "Maybe that one."}))
		if end.event != "waiting_for_user" {
			t.Fatalf("%s: %s %v", c.name, end.event, end.data)
		}
		resp := respond(t, base, map[string]any{"conversation_id": conv, "tool_use_id": end.data["tool_use_id"], "message_id": "m2", "selected": []string{c.answer}})
		resp.Body.Close()
		if end, _ := ended(t, tail(t, base, "m2")); end.event != "done" {
			t.Fatalf("%s: the resumed turn completes: %s %v", c.name, end.event, end.data)
		}
		if list := storedShortlist(t, base, conv); len(list) != 0 {
			t.Errorf("%s: nothing is added, got %+v", c.name, list)
		}
		if got := resultFor(u.messages(t, 1), shortlistToolName); !strings.Contains(got, c.want) {
			t.Errorf("%s: the model reads %s", c.name, got)
		}
	}
}

// The case the guard was checked on: a model that, proposing a project,
// says something about it that is not true. The figure it invents in the
// reply and the one it slips into the pause are redacted; the card shows
// the table's facts. The qualitative claim in the reply is not a figure, so
// no signature sees it: that part is the model's prose.
func TestGuardOnAProposalThatInventsFacts(t *testing.T) {
	u := &upstream{streams: []string{
		chunk(`{"role":"assistant","content":"Bishan is a non-mature estate, so prices could rise 15%."}`) +
			chunk(callDelta(0, "call_s", shortlistToolName, `{"project_name":"kestrel gardens","reason":"20% upside."}`)) + finish("tool_calls"),
	}}
	llm := httptest.NewServer(u)
	t.Cleanup(llm.Close)
	deps, checkpoints, err := buildDeps()
	if err != nil {
		t.Fatal(err)
	}
	deps.Guard = newGuard()
	ag, err := dialogram.New(dialogram.Settings{Provider: dialogram.ProviderOpenAI, OpenAIAPIKey: "test", OpenAIBaseURL: llm.URL, OpenAIModel: "m"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app{ag: ag, exec: deps.Executor, checkpoints: checkpoints, buf: streambuf.NewMemStore(0)}.routes())
	t.Cleanup(srv.Close)

	frames := send(t, srv.URL, "m1", map[string]any{"message": "Add Kestrel Gardens."})
	var text string
	for _, f := range frames {
		if f.event == "text" {
			text += f.data["delta"].(string)
		}
	}
	if strings.Contains(text, "15%") || !strings.Contains(text, agent.DefaultPlaceholder) {
		t.Errorf("the invented figure in the reply is redacted: %q", text)
	}
	if !strings.Contains(text, "non-mature") {
		t.Errorf("the guard is not expected to see a qualitative claim; if it now does, update this test: %q", text)
	}
	end, conv := ended(t, frames)
	if in, _ := json.Marshal(end.data["input"]); end.event != "waiting_for_user" || strings.Contains(string(in), "20%") {
		t.Errorf("the figure slipped into the pause is redacted: %s %s", end.event, in)
	}
	if card := end.data["message"].(string); !strings.Contains(card, "District 20") || strings.Contains(card, "%") {
		t.Errorf("the card shows the table's facts: %q", card)
	}
	for _, turn := range storedTurns(t, srv.URL, conv) {
		for _, b := range turn.Blocks {
			if strings.Contains(b.Text+string(b.ToolInput), "%") {
				t.Errorf("the stored conversation keeps a redacted figure: %+v", b)
			}
		}
	}
}
