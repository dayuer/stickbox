package store

import (
	"database/sql"
	"errors"
)

// TestRun 是适配测试的一轮（API 文档 4.9）。
//
// Verdict 为空表示这一轮还开着。同一台设备同时只允许一轮开着——
// 两轮并行的话，作业会带着互相冲突的 quirk 覆盖下去，出来的纸没法归因。
type TestRun struct {
	RunID      string
	Dev        string
	Serial     string
	Test       string
	Variant    string // JSON
	JobsNeeded int
	Created    int64
	Verdict    string
	Answered   int64
}

const testRunCols = `run_id,dev,serial,test,variant,jobs_needed,created,verdict,answered`

func (s *Store) InsertTestRun(r TestRun) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO test_runs(`+testRunCols+`) VALUES(?,?,?,?,?,?,?,'',0)`,
		r.RunID, r.Dev, r.Serial, r.Test, r.Variant, r.JobsNeeded, r.Created)
	return err
}

func (s *Store) GetTestRun(runID string) (TestRun, bool, error) {
	var r TestRun
	err := s.db.QueryRow(`SELECT `+testRunCols+` FROM test_runs WHERE run_id=?`, runID).
		Scan(&r.RunID, &r.Dev, &r.Serial, &r.Test, &r.Variant, &r.JobsNeeded,
			&r.Created, &r.Verdict, &r.Answered)
	if errors.Is(err, sql.ErrNoRows) {
		return TestRun{}, false, nil
	}
	return r, err == nil, err
}

// OpenTestRun 返回这台设备上还开着的那一轮。
func (s *Store) OpenTestRun(dev string) (TestRun, bool, error) {
	var r TestRun
	err := s.db.QueryRow(
		`SELECT `+testRunCols+` FROM test_runs WHERE dev=? AND verdict=''
		  ORDER BY created DESC LIMIT 1`, dev).
		Scan(&r.RunID, &r.Dev, &r.Serial, &r.Test, &r.Variant, &r.JobsNeeded,
			&r.Created, &r.Verdict, &r.Answered)
	if errors.Is(err, sql.ErrNoRows) {
		return TestRun{}, false, nil
	}
	return r, err == nil, err
}

// AnswerTestRun 记下结论并关闭这一轮。verdict 不能为空——空串是「还开着」。
func (s *Store) AnswerTestRun(runID, verdict string, at int64) error {
	if verdict == "" {
		return errors.New("store: verdict 不能为空")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`UPDATE test_runs SET verdict=?, answered=? WHERE run_id=? AND verdict=''`,
		verdict, at, runID)
	return err
}

// OpenRunIDs 返回这台设备上还开着的那些轮，按测试 id 索引。
//
// GET /tests 要用它把 run_id 一起带给 App：只报 state=running 而不给
// run_id 的话，App 重启之后就再也没法中止那一轮——而一轮开着会挡住
// 之后所有测试（409），用户被永久卡死且没有出路。
func (s *Store) OpenRunIDs(dev string) (map[string]string, error) {
	rows, err := s.db.Query(
		`SELECT test, run_id FROM test_runs WHERE dev=? AND verdict='' ORDER BY created ASC`,
		dev)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var test, id string
		if err := rows.Scan(&test, &id); err != nil {
			return nil, err
		}
		out[test] = id
	}
	return out, rows.Err()
}

// LatestVerdicts 返回这台设备每个测试最近一次的结论。
// 还开着的那一轮 verdict 是空串，调用方据此显示 running。
func (s *Store) LatestVerdicts(dev string) (map[string]string, error) {
	rows, err := s.db.Query(
		`SELECT test, verdict FROM test_runs WHERE dev=? ORDER BY created ASC`, dev)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var test, verdict string
		if err := rows.Scan(&test, &verdict); err != nil {
			return nil, err
		}
		out[test] = verdict // 按时间升序扫，后面的覆盖前面的
	}
	return out, rows.Err()
}
