package realtime_test

import (
	"github.com/ably/ably-go"
	"github.com/ably/ably-go/realtime"
	"github.com/ably/ably-go/test/support"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"testing"
)

func TestRealtime(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Realtime Suite")
}

var (
	testApp *support.TestApp
	client  *realtime.Client
	channel *realtime.Channel
)

var _ = BeforeSuite(func() {
	testApp = support.NewTestApp()
	_, err := testApp.Create()
	Expect(err).NotTo(HaveOccurred())
})

var _ = BeforeEach(func() {
	client = ably.NewRealtimeClient(testApp.ClientOptions)
	channel = client.Channel("test")
})

var _ = AfterSuite(func() {
	_, err := testApp.Delete()
	Expect(err).NotTo(HaveOccurred())
})
