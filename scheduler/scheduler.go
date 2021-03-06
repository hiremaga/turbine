package scheduler

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/concourse/turbine/api/builds"
	"github.com/concourse/turbine/builder"
	"github.com/pivotal-golang/lager"
)

type Scheduler interface {
	Start(builds.Build)
	Attach(builder.RunningBuild)
	Abort(guid string)

	Drain() []builder.RunningBuild
}

type scheduler struct {
	logger lager.Logger

	builder builder.Builder

	httpClient *http.Client

	inFlight *sync.WaitGroup
	draining chan struct{}
	running  map[*builder.RunningBuild]bool
	aborting map[string]chan struct{}

	mutex *sync.RWMutex
}

func NewScheduler(l lager.Logger, b builder.Builder) Scheduler {
	return &scheduler{
		logger: l,

		builder: b,

		httpClient: &http.Client{
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		},

		inFlight: new(sync.WaitGroup),
		draining: make(chan struct{}),
		running:  make(map[*builder.RunningBuild]bool),
		aborting: make(map[string]chan struct{}),

		mutex: new(sync.RWMutex),
	}
}

func (scheduler *scheduler) Drain() []builder.RunningBuild {
	close(scheduler.draining)
	scheduler.inFlight.Wait()
	return scheduler.runningBuilds()
}

func (scheduler *scheduler) Start(build builds.Build) {
	scheduler.inFlight.Add(1)

	log := scheduler.logger.Session("start", lager.Data{
		"build": build,
	})

	abort := scheduler.abortChannel(build.Guid)

	started, errored := scheduler.builder.Start(build, abort)

	go func() {
		select {
		case running := <-started:
			log.Info("started")

			running.Build.Status = builds.StatusStarted
			scheduler.reportBuild(running.Build, log)

			scheduler.Attach(running)
		case err := <-errored:
			log.Error("errored", err)

			build.Status = builds.StatusErrored
			scheduler.reportBuild(build, log)
		}

		scheduler.unregisterAbortChannel(build.Guid)
		scheduler.inFlight.Done()
	}()
}

func (scheduler *scheduler) Attach(running builder.RunningBuild) {
	scheduler.inFlight.Add(1) // in addition to .Start's
	defer scheduler.inFlight.Done()

	runningRef := &running

	scheduler.addRunning(runningRef)

	abort := scheduler.abortChannel(running.Build.Guid)
	defer scheduler.unregisterAbortChannel(running.Build.Guid)

	succeeded, failed, errored := scheduler.builder.Attach(running, abort)

	log := scheduler.logger.Session("attach", lager.Data{
		"build": running.Build,
	})

	select {
	case err := <-failed:
		log.Error("failed", err)

		running.Build.Status = builds.StatusFailed
		scheduler.reportBuild(running.Build, log)

	case succeededBuild := <-succeeded:
		log.Info("succeeded")

		scheduler.complete(succeededBuild)

	case err := <-errored:
		log.Error("errored", err)

		running.Build.Status = builds.StatusErrored
		scheduler.reportBuild(running.Build, log)

	case <-scheduler.draining:
		return
	}

	scheduler.removeRunning(runningRef)
}

func (scheduler *scheduler) Abort(guid string) {
	scheduler.abortBuild(guid)
}

func (scheduler *scheduler) complete(succeeded builder.SucceededBuild) {
	abort := scheduler.abortChannel(succeeded.Build.Guid)
	finished, errored := scheduler.builder.Complete(succeeded, abort)

	log := scheduler.logger.Session("complete", lager.Data{
		"build": succeeded.Build,
	})

	select {
	case finishedBuild := <-finished:
		log.Info("completed")

		finishedBuild.Status = builds.StatusSucceeded
		scheduler.reportBuild(finishedBuild, log)

	case err := <-errored:
		log.Error("failed", err)

		succeeded.Build.Status = builds.StatusErrored
		scheduler.reportBuild(succeeded.Build, log)
	}
}

func (scheduler *scheduler) runningBuilds() []builder.RunningBuild {
	scheduler.mutex.RLock()

	running := []builder.RunningBuild{}
	for build, _ := range scheduler.running {
		running = append(running, *build)
	}

	scheduler.mutex.RUnlock()

	return running
}

func (scheduler *scheduler) addRunning(running *builder.RunningBuild) {
	scheduler.mutex.Lock()
	scheduler.running[running] = true
	scheduler.mutex.Unlock()
}

func (scheduler *scheduler) removeRunning(running *builder.RunningBuild) {
	scheduler.mutex.Lock()
	delete(scheduler.running, running)
	scheduler.mutex.Unlock()
}

func (scheduler *scheduler) abortChannel(guid string) chan struct{} {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()

	abort, found := scheduler.aborting[guid]
	if !found {
		abort = make(chan struct{})
		scheduler.aborting[guid] = abort
	}

	return abort
}

func (scheduler *scheduler) unregisterAbortChannel(guid string) {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()

	delete(scheduler.aborting, guid)
}

func (scheduler *scheduler) abortBuild(guid string) {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()

	abort, found := scheduler.aborting[guid]
	if !found {
		return
	}

	close(abort)
}

func (scheduler *scheduler) runBuild(originalBuild builds.Build, started <-chan builder.RunningBuild, errored <-chan error) {
}

func (scheduler *scheduler) reportBuild(build builds.Build, logger lager.Logger) {
	if build.Callback == "" {
		return
	}

	log := logger.Session("report", lager.Data{
		"build": build,
	})

	// this should always successfully parse (it's done via validation)
	destination, _ := url.ParseRequestURI(build.Callback)

	payload, _ := json.Marshal(build)

	for {
		res, err := scheduler.httpClient.Do(&http.Request{
			Method: "PUT",
			URL:    destination,

			ContentLength: int64(len(payload)),

			Header: map[string][]string{
				"Content-Type": {"application/json"},
			},

			Body: ioutil.NopCloser(bytes.NewBuffer(payload)),
		})

		if err != nil {
			log.Error("failed", err)
			time.Sleep(time.Second)
			continue
		}

		res.Body.Close()

		break
	}
}
