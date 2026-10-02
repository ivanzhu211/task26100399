// Package prelabel 提供预标注协调能力：管理文本记录、模型轮次与人工标注状态。
package prelabel

import "sync"

// PredictFunc 是调用方提供的同步批预测函数：接收文本，返回对应标签。
type PredictFunc func(texts []string) []string

// Status 表示记录对外可见的标注状态。
type Status int

const (
	StatusPending Status = iota // 待预测：本轮尚无机器预测，也无人工结果
	StatusMachine               // 当前轮次的机器预测
	StatusHuman                 // 人工结果
)

// RecordView 是查看操作返回的记录快照，可供后续人工提交引用。
type RecordView struct {
	ID     int
	Text   string
	Status Status
	Label  string // StatusMachine 时为机器预测，StatusHuman 时为人工标签，否则为空

	round        uint64 // 查看时的模型轮次
	humanVersion uint64 // 查看时的人工状态版本
}

// record 是协调器内部保存的单条记录状态。
type record struct {
	text         string
	humanLabel   string
	hasHuman     bool
	humanVersion uint64
	machineLabel string
	machineRound uint64 // 0 表示无有效机器预测
}

// Coordinator 是预标注协调器，所有状态保存在进程内存中。
type Coordinator struct {
	mu      sync.Mutex
	records []record
	models  map[string]PredictFunc
	round   uint64
	wg      sync.WaitGroup // 跟踪在途预测调用，供 Wait 使用
}

// New 以固定的文本记录和可用模型创建协调器。
func New(texts []string, models map[string]PredictFunc) *Coordinator {
	records := make([]record, len(texts))
	for i, t := range texts {
		records[i].text = t
	}
	ms := make(map[string]PredictFunc, len(models))
	for name, fn := range models {
		ms[name] = fn
	}
	return &Coordinator{records: records, models: ms}
}

// EnableModel 启用模型并开启新轮次，自动对尚无人工结果的记录推进预测。
// 每次启用（包括连续启用同一模型或回滚）都是新轮次；旧轮次结果一律作废。
func (c *Coordinator) EnableModel(name string) {
	fn, ok := c.models[name]
	if !ok {
		panic("prelabel: unknown model " + name)
	}
	c.mu.Lock()
	c.round++
	round := c.round
	var ids []int
	var texts []string
	for i := range c.records {
		if !c.records[i].hasHuman {
			ids = append(ids, i)
			texts = append(texts, c.records[i].text)
		}
	}
	c.wg.Add(1)
	c.mu.Unlock()

	go func() {
		defer c.wg.Done()
		labels := fn(texts)
		c.mu.Lock()
		defer c.mu.Unlock()
		if round != c.round {
			return // 旧轮次结果作废，即使模型与标签相同
		}
		for i, id := range ids {
			if i >= len(labels) {
				break
			}
			if c.records[id].hasHuman {
				continue // 人工结果优先，预测不得覆盖
			}
			c.records[id].machineLabel = labels[i]
			c.records[id].machineRound = round
		}
	}()
}

// View 批量查看记录，返回可供人工提交引用的快照。
func (c *Coordinator) View(ids []int) []RecordView {
	c.mu.Lock()
	defer c.mu.Unlock()
	views := make([]RecordView, len(ids))
	for i, id := range ids {
		r := &c.records[id]
		v := RecordView{
			ID:           id,
			Text:         r.text,
			round:        c.round,
			humanVersion: r.humanVersion,
		}
		switch {
		case r.hasHuman:
			v.Status = StatusHuman
			v.Label = r.humanLabel
		case r.machineRound == c.round && c.round != 0:
			v.Status = StatusMachine
			v.Label = r.machineLabel
		default:
			v.Status = StatusPending
		}
		views[i] = v
	}
	return views
}

// Accept 接受所查看的那一次预测作为人工结果。
// 仅当查看的是机器预测、该预测仍属于当前轮次、且人工状态自查看后未改变时生效。
func (c *Coordinator) Accept(v RecordView) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v.ID < 0 || v.ID >= len(c.records) {
		return false
	}
	r := &c.records[v.ID]
	if v.Status != StatusMachine {
		return false
	}
	if r.humanVersion != v.humanVersion {
		return false // 人工状态自查看后已改变
	}
	if v.round != c.round || r.machineRound != c.round || r.machineLabel != v.Label {
		return false // 所查看的预测已不属于当前轮次
	}
	r.humanLabel = v.Label
	r.hasHuman = true
	r.humanVersion++
	return true
}

// Override 以明确标签进行人工覆盖。
// 不要求查看时的模型轮次仍有效，但要求人工状态自查看后未改变。
func (c *Coordinator) Override(v RecordView, label string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v.ID < 0 || v.ID >= len(c.records) {
		return false
	}
	r := &c.records[v.ID]
	if r.humanVersion != v.humanVersion {
		return false // 人工状态自查看后已改变
	}
	r.humanLabel = label
	r.hasHuman = true
	r.humanVersion++
	return true
}

// Wait 阻塞直到所有在途预测调用返回（主要用于测试与收尾）。
func (c *Coordinator) Wait() {
	c.wg.Wait()
}
