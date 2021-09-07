PROJECT_NAME := minio-test
REGISTRY := k8scc01covidacr.azurecr.io
CONTROLLER := minio-profile-controller
TAG := latest

.DEFAULT: all

minio: 
	docker run \
		-p 9000:9000 \
		-p 9001:9001 \
  		minio/minio server /data --console-address ":9001"

kind:
	###  _   ___           _
	### | | / (_)         | |
	### | |/ / _ _ __   __| |
	### |    \| | '_ \ / _` |
	### | |\  \ | | | | (_| |
	### \_| \_/_|_| |_|\__,_|
	###	
	kind create cluster --name $(PROJECT_NAME) --kubeconfig local-config
	#kubectl cluster-info --context kind-$(PROJECT_NAME)

clean delete:
	kind delete clusters $(PROJECT_NAME)

profile-crd:
	KUBECONFIG=local-config kubectl apply -f https://raw.githubusercontent.com/kubeflow/kubeflow/master/components/profile-controller/config/crd/bases/kubeflow.org_profiles.yaml

controller:
	#docker build . -t $(REGISTRY)/$(CONTROLLER):$(TAG)
	#kind load docker-image $(REGISTRY)/$(CONTROLLER):$(TAG) --name $(PROJECT_NAME)
	kind load docker-image bleepbloop --name $(PROJECT_NAME)

controller-deploy: controller
	kubectl create namespace daaas || true
	kubectl label  namespace daaas istio-injection=enabled --overwrite || true
	IMAGE_SHA=$(TAG) envsubst < deploy/deploy.yaml.tpl > deploy/deploy.yaml
	kubectl apply -f deploy/deploy.yaml

alice bob carla:
	kubectl apply -f profiles/$@.yaml

all: kind profile-crd