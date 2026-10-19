package cli

import (
	"os"
	"strings"
	"testing"
)

// TestReadModelEnvReportsBadTimeout 非法时长必须报出，不得静默回落。
//
// 这是本轮新增的关键不变量：用户写 MEMORY_LLM_TIMEOUT=300（想 300 秒，漏了单位），
// 静默回落成 60 秒的后果是「请求莫名其妙卡 60 秒就断」，
// 而配置里写着 300——**配错的值不报错，返回一个看起来完全正常的结果**。
func TestReadModelEnvReportsBadTimeout(t *testing.T) {
	t.Setenv(envLLMTimeout, "300") // 缺单位
	m := readModelEnv()
	if m.TimeoutError == "" {
		t.Fatal("非法时长必须被记录")
	}
	if !strings.Contains(m.TimeoutError, "300") {
		t.Fatalf("错误信息应含原值，实际: %q", m.TimeoutError)
	}
	if m.Timeout != 0 {
		t.Fatalf("非法时长不应设出有效超时值，实际: %v", m.Timeout)
	}
}

func TestReadModelEnvRejectsNonPositiveTimeout(t *testing.T) {
	t.Setenv(envLLMTimeout, "0s")
	if readModelEnv().TimeoutError == "" {
		t.Fatal("0 超时必须被记录")
	}
}

func TestReadModelEnvAcceptsValidTimeout(t *testing.T) {
	t.Setenv(envLLMTimeout, "45s")
	m := readModelEnv()
	if m.TimeoutError != "" {
		t.Fatalf("合法时长不应报错: %q", m.TimeoutError)
	}
	if m.Timeout.String() != "45s" {
		t.Fatalf("超时值不对: %v", m.Timeout)
	}
}

func TestReadModelEnvEmptyTimeoutIsFine(t *testing.T) {
	t.Setenv(envLLMTimeout, "")
	m := readModelEnv()
	if m.TimeoutError != "" {
		t.Fatalf("未设超时不���报错: %q", m.TimeoutError)
	}
}

func TestModelOptionsConfigured(t *testing.T) {
	if (ModelOptions{BaseURL: "u", Model: "m"}).Configured() != true {
		t.Fatal("两项齐备应算已配置")
	}
	if (ModelOptions{BaseURL: "u"}).Configured() != false {
		t.Fatal("缺 Model 不算已配置")
	}
	if (ModelOptions{Model: "m"}).Configured() != false {
		t.Fatal("缺 BaseURL 不算已配置")
	}
}

// TestExtractRequiresModel 缺模型配置必须显式失败（退出码 2）。
//
// 这条易用性缺口的另一面：未配置模型时若**静默**降级成「提炼出 0 条候选」，
// 用户看到的是一次正常但空洞的结果——而真正原因是压根没配模型。
func TestExtractRequiresModel(t *testing.T) {
	os.Unsetenv(envLLMBaseURL)
	os.Unsetenv(envLLMModel)
	var stdout, stderr strings.Builder
	code := Run(t.Context(), []string{"extract"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("缺模型应返回退出码 2，实际 %d", code)
	}
	if !strings.Contains(stderr.String(), envLLMBaseURL) {
		t.Fatalf("错误信息应说明要设哪个环境变量，实际: %q", stderr.String())
	}
}

func TestPromoteRequiresModel(t *testing.T) {
	os.Unsetenv(envLLMBaseURL)
	os.Unsetenv(envLLMModel)
	var stdout, stderr strings.Builder
	code := Run(t.Context(), []string{"promote"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("缺模型应返回退出码 2，实际 %d", code)
	}
}

// TestExtractRejectsBadTimeoutBeforeTouchingDB 配置错误要在碰库之前报出。
func TestExtractRejectsBadTimeoutBeforeTouchingDB(t *testing.T) {
	t.Setenv(envLLMBaseURL, "http://127.0.0.1:1/v1")
	t.Setenv(envLLMModel, "m")
	t.Setenv(envLLMTimeout, "300")
	var stdout, stderr strings.Builder
	code := Run(t.Context(), []string{"extract", "--db", "/nonexistent/dir/x.sqlite"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("非法超时应返回退出码 2，实际 %d", code)
	}
	if !strings.Contains(stderr.String(), "不是合法的时长") {
		t.Fatalf("应报时长非法，实际: %q", stderr.String())
	}
	// 关键：不应先报「打开失败」——那说明它先碰了库才校验配置。
	if strings.Contains(stderr.String(), "打开失败") {
		t.Fatalf("配置错误应先于库访问报出，实际: %q", stderr.String())
	}
}
