package profile

// 适配测试目录（API 文档 4.8）。
//
// 只收录**文档写明了变体和判定语义**的测试。4.10 的 detail 表里还提到
// iface_cycle，但文档从没说过它下发什么变体、pages_printed 0..5 各自代表
// 什么——照着猜一个出来，得到的数据比没有数据更糟，而且症状（第二份不出、
// 乱码纸）最难归因回「测试判错了」。要加它，先把语义写进文档。
type TestDef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// required 的只有 double_print：单份作业成功不能说明任何事，
	// 不发作业结束符时症状是「第二份不出」，只有连打才测得出来。
	Required   bool     `json:"required"`
	JobsNeeded int      `json:"jobs_needed"`
	Affects    []string `json:"affects"`
	// Instruction 是给用户看的一句话，告诉他该观察什么。
	Instruction string `json:"-"`
}

const (
	TestDoublePrint = "double_print"
	TestWake        = "wake"
	TestMargins     = "margins"
)

var testCatalog = []TestDef{
	{
		ID: TestDoublePrint, Name: "连打两份", Required: true, JobsNeeded: 2,
		Affects:     []string{"uel_job_end"},
		Instruction: "接下来会打印 2 页。请把打印机纸盒装好，然后看实际出了几张纸。",
	},
	{
		ID: TestWake, Name: "休眠唤醒", Required: false, JobsNeeded: 1,
		Affects:     []string{"uel_wake", "wake_delay_ms"},
		Instruction: "请先让打印机进入休眠（面板灯灭），再回来开始。看它会不会醒来出纸。",
	},
	{
		ID: TestMargins, Name: "页边距", Required: false, JobsNeeded: 1,
		Affects:     []string{"margins_mm"},
		Instruction: "接下来会打印一张带刻度的边框页。量一下四边各被切掉多少毫米。",
	},
}

// Catalog 返回测试目录的副本。
func Catalog() []TestDef {
	out := make([]TestDef, len(testCatalog))
	copy(out, testCatalog)
	return out
}

// FindTest 按 id 查。
func FindTest(id string) (TestDef, bool) {
	for _, t := range testCatalog {
		if t.ID == id {
			return t, true
		}
	}
	return TestDef{}, false
}

// VariantFor 返回这个测试要下发的一次性覆盖。
//
// 变体的逻辑一律是「**去掉**那个可能不必要的动作，看还行不行」——
// 而不是加一个动作。这样 pass 的含义永远是「不需要它」，fail 永远是
// 「需要它」，不会因为测试不同而反过来。
//
// 返回的 hooks 只覆盖出现在里面的钩子，没提到的仍按档案执行（文档 3.4）。
func VariantFor(test string) (Hooks, map[string]any, bool) {
	switch test {
	case TestDoublePrint:
		// 去掉作业结束符。空数组不是布尔开关——固件按同一套原语解析。
		return Hooks{JobEnd: []Step{}}, map[string]any{"uel_job_end": false}, true
	case TestWake:
		return Hooks{Wake: []Step{}}, map[string]any{"uel_wake": false}, true
	case TestMargins:
		// 页边距测的是纸，不需要改任何动作。
		return Hooks{}, map[string]any{}, true
	}
	return Hooks{}, nil, false
}
