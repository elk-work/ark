package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elk-work/ark/internal/workrecord"
)

func TestPostEventsSplitsAtIngestCap(t *testing.T) {
	cases := []struct {
		n    int
		want []int
	}{
		{0, nil},
		{1, []int{1}},
		{elkMaxBatch, []int{elkMaxBatch}},
		{elkMaxBatch + 1, []int{elkMaxBatch, 1}},
		{elkMaxBatch * 2, []int{elkMaxBatch, elkMaxBatch}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.n), func(t *testing.T) {
			var got []int
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if gotAuth := r.Header.Get("Authorization"); gotAuth != "Bearer tok" {
					t.Errorf("Authorization = %q", gotAuth)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				var batch []workrecord.Event
				if err := json.Unmarshal(body, &batch); err != nil {
					t.Fatal(err)
				}
				if len(batch) > elkMaxBatch {
					http.Error(w, fmt.Sprintf(`{"error":"no valid events","dropped":["batch of %d exceeds cap %d; split and resend"]}`, len(batch), elkMaxBatch), http.StatusBadRequest)
					return
				}
				got = append(got, len(batch))
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"accepted":%d,"results":[]}`, len(batch))
			}))
			defer ts.Close()

			events := make([]workrecord.Event, tc.n)
			for i := range events {
				events[i].ExternalID = fmt.Sprintf("e%d", i)
			}
			res, err := postEvents(context.Background(), ts.URL, "tok", events)
			if err != nil {
				t.Fatalf("postEvents: %v", err)
			}
			if res.Endpoint != ts.URL {
				t.Errorf("endpoint = %q", res.Endpoint)
			}
			if res.Accepted != tc.n {
				t.Errorf("accepted = %d, want %d", res.Accepted, tc.n)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("batches = %v, want %v", got, tc.want)
			}
			for i, n := range tc.want {
				if got[i] != n {
					t.Fatalf("batches = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
