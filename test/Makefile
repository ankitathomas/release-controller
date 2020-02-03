all: deploy
.PHONY: all

deploy:
	oc create -f deploy.yaml
.PHONY: deploy

undeploy:
	-oc delete -f deploy.yaml
.PHONY: undeploy

clean: undeploy
	-oc delete -n release-controller-test-job jobs --all
	-oc delete -n release-controller-test-job prowjobs --all
	-oc delete -n release-controller-test-job statefulsets --all
	-oc delete -n release-controller-test-release is --all
.PHONY: clean

all-releases:
	./hack/generate-releases.py -a
.PHONY: all-releases

4.1:
	./hack/generate-releases.py -a -v 4.1
.PHONY: 4.1

4.2:
	./hack/generate-releases.py -a -v 4.2
.PHONY: 4.2

4.3:
	./hack/generate-releases.py -a -v 4.3
.PHONY: 4.3

apply:
	sh ./apply.sh
.PHONY: apply

pass:
	./complete.sh "T"
.PHONY: pass

fail:
	./complete.sh "F"
.PHONY: fail

keep:
	./keep
.PHONY: keep
