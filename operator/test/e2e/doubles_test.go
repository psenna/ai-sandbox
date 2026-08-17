package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This spec self-tests the doubles and the MinIO S3 helper directly:
// nothing in the operator consumes any of them yet (see #22's non-goals),
// so this is the only place proving they work correctly, ahead of the
// later issues (#28-#30) that will build on them.
var _ = Describe("platform doubles", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		h.BrokerReset(ctx)
	})

	It("serves broker health and round-trips a programmed CheckSummary", func() {
		resp, err := http.Get(h.brokerBaseURL() + "/healthz")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		h.BrokerSetCheckSummary(ctx, "o/r", "main", "success", CheckRun{Name: "build", Status: "completed", Conclusion: "success"})

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.brokerBaseURL()+"/o/r/checks/main", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+h.Cfg.BrokerToken)
		resp2, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp2.Body.Close()
		Expect(resp2.StatusCode).To(Equal(http.StatusOK))

		var got CheckSummary
		Expect(json.NewDecoder(resp2.Body).Decode(&got)).To(Succeed())
		Expect(got.Overall).To(Equal("success"))
		Expect(got.Checks).To(HaveLen(1))
		Expect(got.Checks[0].Name).To(Equal("build"))
	})

	It("rejects a broker request without the Bearer token", func() {
		resp, err := http.Get(h.brokerBaseURL() + "/o/r/checks/main")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("serves model health and a canned Messages API reply", func() {
		h.ModelSetReply(ctx, "e2e canary reply")

		resp, err := http.Post(h.modelBaseURL()+"/v1/messages", "application/json", nil) //nolint:noctx // deliberately simple; POST needs no request body here
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		var got struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
		Expect(got.Content).To(HaveLen(1))
		Expect(got.Content[0].Text).To(Equal("e2e canary reply"))
	})

	It("round-trips an object through the MinIO fixture bucket", func() {
		key := "doubles-test/" + randHex(4) + ".txt"
		Expect(h.S3Put(ctx, key, []byte("hello e2e"))).To(Succeed())

		data := h.WaitForS3Object(ctx, h.Cfg.FixtureBucket, key, 30*time.Second)
		Expect(string(data)).To(Equal("hello e2e"))

		objs, err := h.S3List(ctx, h.Cfg.FixtureBucket, "doubles-test/")
		Expect(err).NotTo(HaveOccurred())
		var found bool
		for _, o := range objs {
			if o.Key == key {
				found = true
			}
		}
		Expect(found).To(BeTrue(), "S3List did not include %s", key)
	})
})
