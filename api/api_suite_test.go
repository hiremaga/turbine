package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/concourse/turbine/api"
	"github.com/concourse/turbine/resource/fakes"
	"github.com/concourse/turbine/routes"
	"github.com/concourse/turbine/scheduler/fakescheduler"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/rata"
)

var scheduler *fakescheduler.FakeScheduler
var tracker *fakes.FakeTracker
var drain chan struct{}

var server *httptest.Server
var client *http.Client

var _ = BeforeEach(func() {
	scheduler = fakescheduler.New()
	tracker = new(fakes.FakeTracker)
	drain = make(chan struct{})

	turbineEndpoint := rata.NewRequestGenerator("http://some-turbine", routes.Routes)

	handler, err := api.New(lagertest.NewTestLogger("test"), scheduler, tracker, turbineEndpoint, drain)
	Ω(err).ShouldNot(HaveOccurred())

	server = httptest.NewServer(handler)
	client = &http.Client{
		Transport: &http.Transport{},
	}
})

func TestApi(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "API Suite")
}
