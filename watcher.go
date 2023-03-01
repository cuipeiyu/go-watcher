package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/cuipeiyu/go-watcher/walk"
)

type Event struct {
	Op
	FromPath string
	FromInfo os.FileInfo
	ToPath   string
	ToInfo   os.FileInfo
}

func (e *Event) String() string {
	return fmt.Sprintf("[%s] %s => %s", e.Op.String(), e.FromPath, e.ToPath)
}

type Op uint

const (
	Create Op = 1 << iota
	Update
	MoveTo
	Remove
	Rename
	Chmod
	AllOps = Create | Update | MoveTo | Remove | Rename | Chmod
)

var opName = map[Op]string{
	Create: "Create",
	Update: "Update",
	MoveTo: "MoveTo",
	Remove: "Remove",
	Rename: "Rename",
	Chmod:  "Chmod",
	AllOps: "Create|Update|MoveTo|Remove|Rename|Chmod",
}

func (op Op) String() string {
	return opName[op]
}

type Option func(*Watcher)

func WithContext(ctx context.Context) Option {
	return func(w *Watcher) {
		w.ctx = ctx
	}
}

func WithIgnorePattern(pattern string) Option {
	return func(w *Watcher) {
		lines := strings.Split(pattern, "\n")
		rules := []string{}

		for _, line := range lines {
			line = strings.TrimSpace(line)
			if len(line) == 0 || strings.HasPrefix(line, "#") {
				continue
			}

			rules = append(rules, line)
		}

		w.ignoreRules = rules
	}
}

func WithOpFilter(ops Op) Option {
	return func(w *Watcher) {
		w.filter = ops
	}
}

func WithMaxEvents(delta int) Option {
	return func(w *Watcher) {
		w.maxEvents = delta
	}
}

type Watcher struct {
	ctx           context.Context
	rootPath      string
	Events        chan Event
	Errors        chan error
	list          map[string]os.FileInfo
	filter        Op
	mutex         *sync.Mutex
	wg            *sync.WaitGroup
	ignoreRules   []string
	fsnotify      *fsnotify.Watcher
	lastEvent     *Event    // 最后一次输出的事件 用来防止同一对象的同一事件 多次紧接着
	lastEventTime time.Time // 最后一次事件时间
	maxEvents     int       // 单回环最大事件数量 控制 默认 -1不限制
}

func New(path string, opt ...Option) (*Watcher, error) {
	w := &Watcher{
		ctx:       context.Background(),
		Events:    make(chan Event),
		Errors:    make(chan error),
		mutex:     new(sync.Mutex),
		wg:        new(sync.WaitGroup),
		filter:    AllOps,
		maxEvents: -1,
	}

	var err error

	w.rootPath, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	w.fsnotify, err = fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	for _, o := range opt {
		o(w)
	}

	// 准备好后 进行第一次walk
	w.list, err = w.walk(w.rootPath, false)
	if err != nil {
		return nil, err
	}

	return w, nil
}

func (w *Watcher) Start() {
	w.wg.Add(1)
	go w.eventListener()
}

func (w *Watcher) SetMaxEvents(delta int) {
	// 不为 -1 则限制最少1个事件输出
	if delta != -1 && delta < 1 {
		delta = 1
	}
	w.mutex.Lock()
	w.maxEvents = delta
	w.mutex.Unlock()
}

// 过滤事件
func (w *Watcher) SetFilterEvents(ops Op) {
	if ops == 0 {
		ops = AllOps
	}
	w.mutex.Lock()
	w.filter = ops
	w.mutex.Unlock()
}

// 循环处理 fsnotify 事件
func (w *Watcher) eventListener() {
	defer w.wg.Done()

	ticker := time.NewTicker(time.Second)

	eventPool := make(chan fsnotify.Event, 10)
	defer close(eventPool)

	defer ticker.Stop()

	for {

		select {
		case _, ok := <-w.ctx.Done():
			if !ok {
				return // exit
			}

		case event := <-w.fsnotify.Events:

			if event.Name == w.rootPath {
				continue
			}

			// 直接事件 分析完后直接返回
			if event.Op&fsnotify.Chmod == fsnotify.Chmod || event.Op&fsnotify.Write == fsnotify.Write {

				// 事件对象是否存在
				w.mutex.Lock()
				old, found := w.list[event.Name]
				w.mutex.Unlock()
				if !found {
					// TODO 没想到怎么处理这种事件
					// w.Errors <- fmt.Errorf("%s 事件对象 在list中 未找到:%s", event.Op, event.Name)
					continue
				}

				// 重新获取 info
				info, err := os.Stat(event.Name)
				if err != nil {
					w.Errors <- err
					continue
				}

				var op Op

				// 顺便判断事件是否屏蔽
				if event.Op&fsnotify.Chmod == fsnotify.Chmod {
					if w.filter&Chmod == 0 {
						continue
					}
					op = Chmod
				} else {
					if w.filter&Update == 0 {
						continue
					}
					op = Update
				}

				// 判断是否忽略
				if w.ignoreMatcher(event.Name, info.IsDir()) {
					continue
				}

				// 更新 list
				w.mutex.Lock()
				w.list[event.Name] = info
				w.mutex.Unlock()

				// 如果是文件夹发生的更新事件则不上报
				if info.IsDir() {
					continue
				}

				// 发送事件 到通道
				w.newEvent(Event{op, event.Name, old, event.Name, info})

			} else {
				// 将事件累积
				// 交给定时器去执行 pollEvents()
				w.mutex.Lock()
				eventPool <- event
				w.mutex.Unlock()
			}

		case err := <-w.fsnotify.Errors:
			// 说明关闭了监控
			if err == nil {
				return
			}
			{
				w.Errors <- err
			}

			// 执行累积的事件
		case <-ticker.C:
			if len(eventPool) > 0 {
				w.pollEvents()
				// 用完 清空池子
				w.mutex.Lock()
				close(eventPool)
				eventPool = make(chan fsnotify.Event, 10)
				w.mutex.Unlock()
			}
		}
	}
}

func (w *Watcher) pollEvents() {
	var err error

	var tmpCreate = []Event{}
	var tmpUpdate = []Event{}
	var tmpMoveTo = []Event{}
	var tmpRemove = []Event{}
	var tmpRename = []Event{}
	var tmpChmod = []Event{}

	creates := make(map[string]os.FileInfo)
	removes := make(map[string]os.FileInfo)

	// 重新walk
	// fmt.Println("pollEvents 重新walk 开始")
	newList, err := w.walk(w.rootPath, false)
	if err != nil {
		// fmt.Println("pollEvents 重新walk 出错", err)
		w.Errors <- err
		return
	}
	// fmt.Println("pollEvents 重新walk 结束", len(newList))

	w.mutex.Lock()

	// 从旧列表中找新列表中不存在的项
	// fmt.Println("pollEvents 找removes项 开始")
	for path, info := range w.list {
		if _, found := newList[path]; !found {
			removes[path] = info
		}
	}
	// fmt.Println("pollEvents 找removes项 结束")

	// 找出已创建的和改过权限的
	for path, info := range newList {
		oldInfo, found := w.list[path]
		if !found {
			creates[path] = info // 旧列表中没找到 说明是新创建的

			// 追加监控
			if info.IsDir() {
				if err := w.fsnotify.Add(path); err != nil {
					w.Errors <- err
				}
			}

			continue
		}

		// 跳过根目录
		if path == w.rootPath {
			continue
		}

		// 对于文件夹发生的 update 这里同样监控 要过滤掉的话，开发者过滤
		// if oldInfo.ModTime() != info.ModTime() && !info.IsDir() {
		if oldInfo.ModTime() != info.ModTime() {
			tmpUpdate = append(tmpUpdate, Event{Update, path, oldInfo, path, info})
		}
		if oldInfo.Mode() != info.Mode() {
			tmpChmod = append(tmpChmod, Event{Chmod, path, oldInfo, path, info})
		}
	}

	// 更新 list
	w.list = newList

	w.mutex.Unlock()

	// 找出
	for path1, info1 := range removes {
		for path2, info2 := range creates {
			if sameFile(info1, info2) {

				rename := false
				e := Event{MoveTo, path1, info1, path2, info2}
				// 如果它们来自同一个目录，则是重命名，而不是移动事件。
				if filepath.Dir(path1) == filepath.Dir(path2) {
					e.Op = Rename
					rename = true
				}

				delete(removes, path1)
				delete(creates, path2)

				if rename {
					tmpRename = append(tmpRename, e)
				} else {
					tmpMoveTo = append(tmpMoveTo, e)
				}
			}
		}
	}

	for path, info := range creates {
		tmpCreate = append(tmpCreate, Event{Create, path, info, path, info})
	}

	for path, info := range removes {
		tmpRemove = append(tmpRemove, Event{Remove, path, info, path, info})
	}

	// 事件计数
	count := 0

	// 事件排序
	if len(tmpRename) > 0 && w.filter&Rename == Rename {
		for _, tmp := range tmpRename {
			if w.maxEvents > 0 && count >= w.maxEvents {
				return // 达到上限 结束
			}
			if w.newEvent(tmp) {
				count++
			}
		}
	}
	if len(tmpMoveTo) > 0 && w.filter&MoveTo == MoveTo {
		for _, tmp := range tmpMoveTo {
			if w.maxEvents > 0 && count >= w.maxEvents {
				return // 达到上限 结束
			}
			if w.newEvent(tmp) {
				count++
			}
		}
	}
	if len(tmpRemove) > 0 && w.filter&Remove == Remove {
		for _, tmp := range tmpRemove {
			if w.maxEvents > 0 && count >= w.maxEvents {
				return // 达到上限 结束
			}
			if w.newEvent(tmp) {
				count++
			}
		}
	}
	if len(tmpUpdate) > 0 && w.filter&Update == Update {
		for _, tmp := range tmpUpdate {
			if w.maxEvents > 0 && count >= w.maxEvents {
				return // 达到上限 结束
			}
			if w.newEvent(tmp) {
				count++
			}
		}
	}
	if len(tmpChmod) > 0 && w.filter&Chmod == Chmod {
		for _, tmp := range tmpChmod {
			if w.maxEvents > 0 && count >= w.maxEvents {
				return // 达到上限 结束
			}
			if w.newEvent(tmp) {
				count++
			}
		}
	}
	if len(tmpCreate) > 0 && w.filter&Create == Create {
		for _, tmp := range tmpCreate {
			if w.maxEvents > 0 && count >= w.maxEvents {
				return // 达到上限 结束
			}
			if w.newEvent(tmp) {
				count++
			}
		}
	}

}

// 返回 是否已添加
func (w *Watcher) newEvent(event Event) bool {

	w.mutex.Lock()
	defer w.mutex.Unlock()

	if w.lastEvent != nil {
		// 时间在可判断范围内
		if time.Since(w.lastEventTime) < time.Second {
			// fmt.Println("时间在可判断范围内")
			// fmt.Printf("       op: %s -> %s\n", w.lastEvent.Op, event.Op)
			// fmt.Printf("from path: %s -> %s\n", w.lastEvent.FromPath, event.FromPath)
			// fmt.Printf("  to path: %s -> %s\n", w.lastEvent.ToPath, event.ToPath)
			// 和本次事件对比
			// 如果和本次事件一模一样那么就不用触发
			if w.lastEvent.Op == event.Op && w.lastEvent.FromPath == event.FromPath && w.lastEvent.ToPath == event.ToPath {
				// fmt.Println("一样 跳过")
				w.lastEventTime = time.Now()
				return false
			}
		}
		w.lastEvent = nil
	}

	w.Events <- event
	w.lastEvent = &event
	w.lastEventTime = time.Now()

	return true
}

func (w *Watcher) ignoreMatcher(path string, isDir bool) bool {
	if w.ignoreRules != nil {
		// to relative path
		path = strings.Replace(path, w.rootPath, "", 1)
		path = strings.TrimLeft(path, "/")

		for _, rule := range w.ignoreRules {
			if matched, _ := filepath.Match(rule, path); matched {
				return true
			}
		}
	}
	return false
}

func (w *Watcher) walk(name string, once bool) (map[string]os.FileInfo, error) {
	tmp := make(map[string]os.FileInfo)

	walkFunc := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// 判断是否忽略
		if w.ignoreMatcher(path, info.IsDir()) {
			if info.IsDir() {
				return walk.SkipDir
			}
			return nil
		}

		tmp[path] = info

		// 添加监控
		// TODO 会出现重复添加
		if info.IsDir() {
			if err := w.fsnotify.Add(path); err != nil {
				w.Errors <- err
			}
		}

		return nil
	}

	if once {
		return tmp, walk.WalkOnce(name, walkFunc)
	}

	return tmp, walk.WalkPath(name, walkFunc)
}

// 获取已监控列表
func (w *Watcher) WatchedList() map[string]os.FileInfo {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	tmp := make(map[string]os.FileInfo)

	for k, v := range w.list {
		tmp[k] = v
	}

	return tmp
}

// watcher 关闭
func (w *Watcher) Close() error {

	//
	err := w.fsnotify.Close()
	if err != nil {
		return err
	}

	w.wg.Wait()

	return nil
}
