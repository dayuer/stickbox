package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/profile"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

// 适配测试（API 文档 4.8 / 4.9 / 4.10）。
//
// 这套东西存在的理由：USB 层的怪癖没法从机型资料查出来，只能出纸看。
// 而「出纸看」这件事只有用户能做——服务端和设备都看不见纸。所以流程是
// 服务端出变体、用户看结果、结论回流成档案，再经投票扩散到同型号。

// attachedSerial 返回这台桥当前插着的打印机序列号。
func (a *API) attachedSerial(dev string) string {
	if act := a.reg.Actor(dev); act != nil {
		return act.Serial()
	}
	return ""
}

// handleTests 是 4.8：测试清单。
func (a *API) handleTests(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	dev := r.PathValue("dev")
	if !a.ownDevice(id, dev) {
		fail(w, 403, "forbidden", "")
		return
	}

	verdicts, err := a.store.LatestVerdicts(dev)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}

	type row struct {
		profile.TestDef
		State string `json:"state"`
	}
	out := []row{}
	for _, t := range profile.Catalog() {
		st := "untested"
		if v, ok := verdicts[t.ID]; ok {
			// 空 verdict 表示那一轮还开着。
			if v == "" {
				st = "running"
			} else {
				st = profile.StateFor(v)
			}
		}
		out = append(out, row{TestDef: t, State: st})
	}
	writeJSON(w, 200, map[string]any{"tests": out})
}

// baselineOK 判断当前生效配置本身能不能打出来。
//
// 判据是「这台打印机最近一件有结论的作业是不是失败」。在一个本来就坏的
// 链路上测变体，得到的全是噪音——用户会把「基线就是坏的」误判成「这个
// 变体不行」，然后把一个错误结论投进众包库。
func (a *API) baselineOK(dev, serial string) (bool, string) {
	jobs, err := a.store.JobsForDevice(dev, 15)
	if err != nil {
		return true, "" // 查不出来就别拦着用户
	}
	for _, j := range jobs {
		if j.Serial != serial {
			continue
		}
		switch j.State {
		case store.StateDone:
			return true, ""
		case store.StateFailed:
			return false, "这台打印机最近一件作业就失败了（" + j.Err +
				"）。先让一份普通文档打出来，再测变体——" +
				"在坏掉的链路上测，得到的全是噪音。"
		}
	}
	return true, "" // 没打过东西，没有反证
}

// handleTestStart 是 4.9：开始一轮测试。
func (a *API) handleTestStart(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	dev := r.PathValue("dev")
	if !a.ownDevice(id, dev) {
		fail(w, 403, "forbidden", "")
		return
	}
	def, ok := profile.FindTest(r.PathValue("test"))
	if !ok {
		fail(w, 404, "not found", "没有这个测试")
		return
	}

	// 同一台设备同时只允许一轮：两轮并行的话，作业会带着互相冲突的
	// quirk 覆盖下去，出来的纸没法归因到任何一个变体。
	if open, exists, err := a.store.OpenTestRun(dev); err != nil {
		fail(w, 500, "server error", "")
		return
	} else if exists {
		fail(w, 409, "test running", "这台设备已有一轮测试在进行中："+open.Test)
		return
	}

	serial := a.attachedSerial(dev)
	if serial == "" {
		fail(w, 404, "not found", "设备上没插打印机，测不了")
		return
	}

	hooks, variant, ok := profile.VariantFor(def.ID)
	if !ok {
		fail(w, 500, "server error", "")
		return
	}
	hooksJSON, err := json.Marshal(hooks)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}

	run := store.TestRun{
		RunID:      newJobID(), // 同样是 12 位十六进制，格式与作业 ID 一致
		Dev:        dev,
		Serial:     serial,
		Test:       def.ID,
		Variant:    string(hooksJSON),
		JobsNeeded: def.JobsNeeded,
		Created:    time.Now().Unix(),
	}
	if err := a.store.InsertTestRun(run); err != nil {
		fail(w, 500, "server error", "")
		return
	}

	okBase, hint := a.baselineOK(dev, serial)
	resp := map[string]any{
		"run_id":      run.RunID,
		"jobs_needed": def.JobsNeeded,
		"variant":     variant,
		"baseline_ok": okBase,
		"instruction": def.Instruction,
	}
	if hint != "" {
		resp["hint"] = hint
	}
	writeJSON(w, 200, resp)
}

// answerReq 是 4.10 的请求体。
type answerReq struct {
	Verdict string         `json:"verdict"`
	Detail  map[string]any `json:"detail"`
}

// handleTestAnswer 是 4.10：提交用户判断。
func (a *API) handleTestAnswer(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	var req answerReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !profile.ValidVerdict(req.Verdict) {
		fail(w, 400, "bad request", "verdict 只能是 pass / fail / unclear / aborted")
		return
	}

	run, exists, err := a.store.GetTestRun(r.PathValue("run_id"))
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !exists {
		fail(w, 404, "not found", "")
		return
	}
	if !a.ownDevice(id, run.Dev) {
		fail(w, 403, "forbidden", "")
		return
	}
	if run.Verdict != "" {
		fail(w, 409, "already answered", "这一轮已经有结论了")
		return
	}

	printer, _, _ := a.store.GetPrinter(run.Serial)
	base := profile.Lookup(a.store, run.Serial, printer.VID, printer.PID, printer.Model)

	out, aerr := profile.Apply(base, run.Test, req.Verdict, req.Detail)
	if aerr != nil {
		fail(w, 400, "bad request", aerr.Error())
		return
	}

	if err := a.store.AnswerTestRun(run.RunID, req.Verdict, time.Now().Unix()); err != nil {
		fail(w, 500, "server error", "")
		return
	}

	resp := map[string]any{
		"applied":  out.Applied,
		"scope":    store.ScopeSerial,
		"votes":    0,
		"promoted": false,
		"message":  "已记下你的判断。",
	}

	// unclear / aborted 什么都不改。文档 4.10：用户不确定时必须让他选
	// unclear，猜出来的数据比没有数据更糟——那句话的落点就在这里。
	if len(out.Applied) == 0 {
		writeJSON(w, 200, resp)
		return
	}

	votes, promoted := a.persistVerdict(run, printer, out)
	resp["votes"] = votes
	resp["promoted"] = promoted
	if promoted {
		resp["message"] = "已保存。同型号已有 " + itoa(votes) +
			" 台得出相同结论，这份配置成为该机型的默认。"
	} else {
		resp["message"] = "已针对你的打印机保存。再有 " +
			itoa(profile.ModelVoteThreshold-votes) +
			" 台同型号得出相同结论后，将成为该机型的默认配置。"
	}
	writeJSON(w, 200, resp)
}

// persistVerdict 把结论写成 serial 级档案，并按投票决定要不要提升到 model 级。
//
// 结论写入后立即重新下发（文档 4.10 结尾），设备不用重启。
func (a *API) persistVerdict(
	run store.TestRun, printer store.Printer, out profile.Outcome,
) (votes int, promoted bool) {
	body, err := profile.Marshal(out.Profile)
	if err != nil {
		slog.Error("档案编码失败", "serial", run.Serial, "err", err)
		return 0, false
	}
	margins := marginsJSON(out.MarginsMM)

	row := store.ProfileRow{
		Scope: store.ScopeSerial, Match: run.Serial,
		Body: string(body), MarginsMM: margins, Votes: 1,
	}
	if err := a.store.PutProfile(row); err != nil {
		slog.Error("保存 serial 级档案失败", "serial", run.Serial, "err", err)
		return 0, false
	}

	// 同型号投票：结论一致就加一票，不一致就标记分歧并回退到下一层。
	// 一个用户看错纸不能让同型号所有人打不出东西。
	mkey := profile.ModelKey(printer.VID, printer.PID, printer.Model)
	votes = 1
	if prev, ok, _ := a.store.GetProfile(store.ScopeModel, mkey); ok {
		if prev.Body == string(body) {
			votes = prev.Votes + 1
		} else {
			if err := a.store.PutProfile(store.ProfileRow{
				Scope: store.ScopeModel, Match: mkey, Body: prev.Body,
				MarginsMM: prev.MarginsMM, Votes: prev.Votes, Disputed: true,
				Pinned: prev.Pinned,
			}); err != nil {
				slog.Error("标记机型档案分歧失败", "model", mkey, "err", err)
			}
			return 1, false
		}
	}
	if votes >= profile.ModelVoteThreshold {
		if err := a.store.PutProfile(store.ProfileRow{
			Scope: store.ScopeModel, Match: mkey,
			Body: string(body), MarginsMM: margins, Votes: votes,
		}); err == nil {
			promoted = true
		}
	} else if votes > 1 {
		// 票数还不够，但要记下来，否则永远攒不满。
		a.store.PutProfile(store.ProfileRow{
			Scope: store.ScopeModel, Match: mkey,
			Body: string(body), MarginsMM: margins, Votes: votes,
		})
	}

	a.republish(run.Dev, run.Serial, printer)
	return votes, promoted
}

// republish 重新查一遍档案并下发。判定写入后设备不用重启就该生效。
func (a *API) republish(dev, serial string, printer store.Printer) {
	if a.pub == nil {
		return
	}
	prof := profile.Lookup(a.store, serial, printer.VID, printer.PID, printer.Model)
	if err := profile.Validate(prof); err != nil {
		slog.Error("判定后的档案不合法，改发默认档案", "serial", serial, "err", err)
		prof = profile.Default(serial)
	}
	body, err := profile.Marshal(prof)
	if err != nil {
		return
	}
	if err := a.pub.PublishProfile(dev, body); err != nil {
		slog.Error("判定后下发档案失败", "dev", dev, "err", err)
	}
}

// marginsJSON 把页边距编码成 ProfileRow.MarginsMM 的存储形式。
// 空切片存空串——表示「没测过」，而不是「四边都是 0」。
func marginsJSON(mm []int) string {
	if len(mm) != 4 {
		return ""
	}
	b, err := json.Marshal(mm)
	if err != nil {
		return ""
	}
	return string(b)
}

func itoa(n int) string { return strconv.Itoa(n) }
