package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/auth"
	"github.com/auucoder/gptgrok2api-go/internal/config"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

func proofServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		auth:            auth.New("api-secret", "admin-secret", "", false, nil),
		cfg:             config.Config{ImageDataDir: t.TempDir(), RootDir: t.TempDir()},
		schedulerLeases: map[string]map[string]any{},
	}
}

func proofAdminRequest(method, target string, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("X-API-Key", "admin-secret")
	request.Header.Set("Content-Type", "application/json")
	return request
}

// /internal/* 必须 fail-closed：未配置密钥一律拒绝，配置后必须校验密钥。
func TestInternalEndpointsAreFailClosed(t *testing.T) {
	s := proofServer(t)

	call := func(handler func(http.ResponseWriter, *http.Request), target, key string) int {
		request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"model":"gpt-image-2"}`))
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("X-Image-Scheduler-Key", key)
		}
		recorder := httptest.NewRecorder()
		handler(recorder, request)
		return recorder.Code
	}

	// 未配置密钥：一律拒绝，不能因为"忘配"就敞开。
	t.Setenv("GO_IMAGE_SCHEDULER_KEY", "")
	if code := call(s.internalImageScheduler, "/internal/image-scheduler/reserve", ""); code < 400 {
		t.Fatalf("未配置密钥时匿名 reserve 返回 %d，期望被拒绝", code)
	}

	// 配置了密钥：错误的密钥必须拒绝，正确的密钥必须放行。
	t.Setenv("GO_IMAGE_SCHEDULER_KEY", "internal-secret")
	if code := call(s.internalImageScheduler, "/internal/image-scheduler/reserve", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("错误密钥返回 %d，期望 401", code)
	}
	if code := call(s.internalImageScheduler, "/internal/image-scheduler/reserve", "internal-secret"); code != http.StatusOK {
		t.Fatalf("正确密钥返回 %d，期望 200", code)
	}
}

// 回归：图片存储 / 备份测试接口不返回 {result:...} 包装，前端 response.result.ok 必然抛错。
func TestProofAdminTestEndpointsLackResultWrapper(t *testing.T) {
	s := proofServer(t)
	for _, testCase := range []struct {
		name    string
		target  string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"image-storage/test", "/api/image-storage/test", s.imageStorageTest},
		{"image-storage/sync", "/api/image-storage/sync", s.imageStorageSync},
		{"backup/test", "/api/backup/test", s.backupTest},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			testCase.handler(recorder, proofAdminRequest(http.MethodPost, testCase.target, "{}"))
			if recorder.Code != http.StatusOK {
				t.Fatalf("状态码 %d: %s", recorder.Code, recorder.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if _, ok := payload["result"]; !ok {
				t.Fatalf("响应无 result 包装，前端读 response.result.ok 会抛 TypeError：%s", strings.TrimSpace(recorder.Body.String()))
			}
		})
	}
}

// 回归：图库列表不返回 page 字段，前端 meta.page 恒为 undefined → 分页永远停在“第 1 页”。
func TestProofGalleryListHasNoPageField(t *testing.T) {
	s := proofServer(t)
	recorder := httptest.NewRecorder()
	s.adminImages(recorder, proofAdminRequest(http.MethodGet, "/api/images?offset=24&limit=24", ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["page"]; !ok {
		t.Fatalf("响应缺 page 字段，前端 meta.page 恒为 undefined：%s", strings.TrimSpace(recorder.Body.String()))
	}
}

// 回归：page/page_size 完全来自外部。int64 极值曾让 (page-1)*pageSize 回绕成负数，
// 负数既躲过 "start > len" 的钳制、又让 "end > len" 失效，最终以
// slice bounds out of range 的形式 panic 掉请求——客户端拿到的是空响应。
func TestAccountListPaginationSurvivesExtremeInput(t *testing.T) {
	root := t.TempDir()
	repository := store.New(
		filepath.Join(root, "accounts.json"),
		filepath.Join(root, "auth_keys.json"),
		filepath.Join(root, "config.json"),
	)
	server := &Server{
		auth:  auth.New("api-secret", "admin-secret", "", false, repository),
		store: repository,
		cfg:   config.Config{DataDir: root, RootDir: root},
	}
	for _, target := range []string{
		"/api/accounts?page=9223372036854775807",
		"/api/accounts?page=9223372036854775807&page_size=9223372036854775807",
		"/api/accounts?page=1&page_size=9223372036854775807",
		"/api/accounts?page=-1&page_size=-1",
		"/api/accounts?page=0&page_size=0",
	} {
		recorder := httptest.NewRecorder()
		server.accounts(recorder, proofAdminRequest(http.MethodGet, target, ""))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s -> %d %s", target, recorder.Code, strings.TrimSpace(recorder.Body.String()))
		}
	}
}

// 回归：交给 accountPool.Feedback 的错误状态码必须由 upstreamStatus(err) 还原，
// 不得写死字面量。
//
// 写死 502 会把请求域错误（上游审核拦截、参数非法返回 400）记成账号基础设施故障，
// pool.Feedback 随即走 5xx 分支给账号累计失败、标异常并打入指数退避 Cooldown——
// 几次违规请求就能冷却掉整池账号，正是铁律四"故障域严格隔离"要禁止的雪崩。
//
// 池子侧的隔离逻辑已由 TestPoolClientModerationAndBadRequestDoesNotPenalizeAccount
// 守住，但那一层看不见"调用方送来了错误的状态码"，所以守卫必须落在调用点上。
//
// 边界：本守卫只认**内联字面量**这一种写法。若有人先 `status := http.StatusBadGateway`
// 再传变量，它就看不见了。这是刻意取舍——静态数据流分析的成本远超收益，
// 而字面量直传正是这类 bug 自然长出来的形态（历史事故即如此）。
func TestFeedbackStatusCodesComeFromUpstreamStatus(t *testing.T) {
	call := regexp.MustCompile(`\.Feedback\(([^,]+),\s*([^,)]+)\s*,`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range call.FindAllStringSubmatch(string(raw), -1) {
			checked++
			status := strings.TrimSpace(match[2])
			if strings.HasPrefix(status, "http.Status") && status != "http.StatusOK" {
				t.Errorf("%s: Feedback 的状态码写死为 %s；必须用 upstreamStatus(err) 还原真实语义", name, status)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有扫描到任何 Feedback 调用点，守卫已失效")
	}
	t.Logf("已核查 %d 处 Feedback 调用点", checked)
}

// pageBounds 的契约：对任意 int 输入恒返回 0 <= start <= end <= total。
// 有了这条不变量，切片表达式就不需要再判边界——特殊分支从设计里消失。
func TestPageBoundsInvariant(t *testing.T) {
	extremes := []int{math.MinInt64, -1, 0, 1, 2, 7, 500, 501, math.MaxInt32, math.MaxInt64}
	for _, page := range extremes {
		for _, size := range extremes {
			for _, total := range []int{0, 1, 7, 500, 501} {
				start, end := pageBounds(page, size, total)
				if start < 0 || end < start || end > total {
					t.Fatalf("pageBounds(%d, %d, %d) = (%d, %d) 违反不变量", page, size, total, start, end)
				}
			}
		}
	}
}

// 回归：内部调度器的 execute-edit 必须与公开 /v1/images/edits 共用同一套张数上界。
//
// 此前公开端点校验 n<=2，而这条内部路径完全不校验——同一个逻辑操作、两条路径、
// 一条设防一条裸奔。n 没有上界时 generateOpenAIImageData 会
// make([][]map[string]string, n) 并派生 n 个 goroutine，实测 RSS 随 n 线性增长。
func TestSchedulerExecuteEditSharesCountLimit(t *testing.T) {
	t.Setenv("GO_IMAGE_SCHEDULER_KEY", "internal-secret")
	root := t.TempDir()
	repository := store.New(
		filepath.Join(root, "accounts.json"),
		filepath.Join(root, "auth_keys.json"),
		filepath.Join(root, "config.json"),
	)
	server := &Server{
		schedulerLeases: map[string]map[string]any{},
		store:           repository,
		accountPool:     accounts.New(repository),
		openAIImage:     &provider.OpenAIImage{},
		cfg:             config.Config{DataDir: root, RootDir: root},
	}

	reserve := httptest.NewRequest(http.MethodPost, "/internal/image-scheduler/reserve", strings.NewReader(`{"model":"gpt-image-2"}`))
	reserve.Header.Set("Content-Type", "application/json")
	reserve.Header.Set("X-Image-Scheduler-Key", "internal-secret")
	recorder := httptest.NewRecorder()
	server.internalImageScheduler(recorder, reserve)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reserve 失败：%d %s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	id := stringValue(payload["reservation_id"])
	if id == "" {
		t.Fatalf("reservation_id 为空：%s", recorder.Body.String())
	}

	// n 用**字符串**形态给出。JSON 数字形态在 int64 极值处会先退化成 float64，
	// fmt.Sprint 得到 "9.223372036854776e+18"，strconv.Atoi 解析失败后回退成 1——
	// 那是偶然的自保，不是设计。multipart 路径拿到的是原始字符串，Atoi 会成功
	// 解析出 MaxInt64，所以这里按真实向量构造。
	for _, body := range []string{
		`{"model":"gpt-image-2","prompt":"p","n":3}`,                     // 略超编辑上界
		`{"model":"gpt-image-2","prompt":"p","n":100000}`,                // 能撑爆分配的规模
		`{"model":"gpt-image-2","prompt":"p","n":"9223372036854775807"}`, // int64 极值
	} {
		request := httptest.NewRequest(http.MethodPost, "/internal/image-scheduler/"+id+"/execute-edit", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Image-Scheduler-Key", "internal-secret")
		response := httptest.NewRecorder()
		server.internalImageScheduler(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("n 越界必须返回 400，body=%s 实际 %d %s", body, response.Code, response.Body.String())
		}
	}
}
