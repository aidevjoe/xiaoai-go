package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 降级一次成功后要记住档位，不能每次请求都重走失败路径
// （否则每次都要多花一个失败往返，几秒钟就没了）。
func TestDegradeLevelIsSticky(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, bad := body["temperature"]; bad {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"temperature not supported"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Model: "m", Temperature: 0.7})

	// 第一次：完整请求被拒（400）→ 去掉 temperature 成功 = 2 次
	if _, _, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "a"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("首次应为 2 次，实际 %d", attempts)
	}

	// 第二次：记住档位，直接 1 次
	if _, _, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "b"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("第二次应只发 1 次，累计 %d", attempts)
	}
}
