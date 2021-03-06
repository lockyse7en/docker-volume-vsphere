# Copyright 2016 VMware, Inc. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

#
# Makefile for Docker data volume *managed* plugin.
#
# This Makefile assumes it's in ./plugin in the tree and the binaries are already built
# Eventually we will throw away deb/rpm build from ../vmdk_plugin/Makefile and will use only
# managed plugin

#
# Makefile names the plugin simlarly to this (for example) 'msterin/docker-volume-vsphere:0.13-dev'
#
# Make targets:
#    info (helpful info), clean (remove build artefacts), plugin, (build plugin), push (push to dockerhub)
#
# Use the following environment vars to change the behavior :
#
# DOCKER_HUB_REPO - Dockerhub repo to use, in both name forming and in pushing to dockerhub.
#                   Defaults to the result of `whoami` command.
#                   Note: you first need to run 'docker login' in order for 'make push' to succeed
# VERSION_TAG     - How you want the version to look. Default is "current git tag +1 "
# EXTRA_TAG       - additional info you want in tag. Default is "-dev"
#
# To check resulting settings use  "make info"
# examples:
#   make
#          --> resulting name (for my build as of 3/17/17)  msterin/docker-volume-vsphere:0.13-dev
#
# 	DOCKER_HUB_REPO=vmware EXTRA_TAG= VERSION_TAG=latest make
#           --> resulting name vmware/docker-volume-vsphere:latest
#
#   DOCKER_HUB_REPO=cnastorage EXTRA_TAG=-CI make
#           --> resulting name cnastorage/docker-volume-vspehere:0.13-CI
#

# grab the latest commit SHA and related tag from git so we can construct plugin tag
GIT_SHA := $(shell git rev-parse --revs-only --short HEAD)
GIT_TAG := $(shell git describe --tags --abbrev=0 $(GIT_SHA))

# Allow these vars to be suplied in environment
DOCKER_HUB_REPO ?= $(shell whoami)
# "git tag + 1" , so "0.11" becomes "0.12"
VERSION_TAG ?= $(shell echo $(GIT_TAG) | awk -F. '{printf ("%d.%d", $$1, $$2+1)}' )
EXTRA_TAG ?= -dev

# final tag
PLUGIN_TAG := $(VERSION_TAG)$(EXTRA_TAG)

# plugin name - used as a base for full plugin name and container for extracting rootfs
PLUGIN_NAME=$(DOCKER_HUB_REPO)/docker-volume-vsphere

# Binaries we want to pick up from the actual build
BIN_LOC := ../build
BINARY := docker-volume-vsphere

# Tmp docker image used to construct rootfs + our binaries
TMP_IMAGE = $(PLUGIN_NAME):rootfs
# Tmp container used for exporting rootfs from it
TMP_CONTAINER := tempContainer

# default target
all: info clean plugin push

# unconditionally run those
.PHONY: all clean info plugin


info:
	@echo Using the following config:
	@echo DOCKER_HUB_REPO $(DOCKER_HUB_REPO) EXTRA_TAG $(EXTRA_TAG) VERSION_TAG $(VERSION_TAG)
	@echo PLUGIN_NAME $(PLUGIN_NAME):$(PLUGIN_TAG)


clean:
	@echo "=== Cleaning work files, images and plugin(s)..."
	rm -rf ./rootfs
	rm -f $(BINARY)
	-docker plugin rm $(PLUGIN_NAME):$(PLUGIN_TAG) -f
	-docker rmi $(TMP_IMAGE)


plugin:
	@echo "== building Docker image, unpacking to ./rootfs and creating plugin..."
	cp $(BIN_LOC)/$(BINARY) .
	docker build -q -t $(TMP_IMAGE) .
	mkdir -p ./rootfs
	docker create --name $(TMP_CONTAINER) $(TMP_IMAGE)
	docker export  $(TMP_CONTAINER)  | tar -x -C ./rootfs
	docker rm -vf  $(TMP_CONTAINER)
	docker rmi $(TMP_IMAGE)
	@echo "-- Creating  plugin $(PLUGIN_NAME):$(PLUGIN_TAG) ..."
	docker plugin create $(PLUGIN_NAME):$(PLUGIN_TAG) .


push:
	@echo Pushing $(PLUGIN_NAME):$(PLUGIN_TAG)  to dockerhub.io...
	@docker plugin push $(PLUGIN_NAME):$(PLUGIN_TAG) || \
			echo 'Please make sure the plugin is built ("make clean plugin") and you have logged in to docker hub first ("docker login -u $(DOCKER_HUB_REPO)")'
