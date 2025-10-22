# --------------------------------------------------------------------------------
# Submodule Init
# For initializing git submodules, these commands need only be run once at clone time
#
# e.g.: `make git_submodule_init --init --remote --merge`

# TODO Fix needing the tag everywhere

.PHONY: git_submodule_init git_submodule_init_opus git_submodule_init_rtaudiowrapper

# Run me once on clone
git_submodule_init: git_submodule_init_base git_submodule_init_opus git_submodule_init_rtaudiowrapper
	go mod tidy

git_submodule_init_base:
	git submodule init && git submodule update

git_submodule_init_opus:
	cd internal/opus && \
		go mod tidy && \
		go get github.com/klauspost/compress/zstd

git_submodule_init_rtaudiowrapper:
	cd internal/rtaudiowrapper && go mod tidy

# --------------------------------------------------------------------------------
# Submodule Build
# For building the git submodules. Again, needs only be run once, unless developing the submodules
#
#  e.g.: `make git_submodule_build`

.PHONY: git_submodule_build git_submodule_build_opus git_submodule_build_rtaudiowrapper

git_submodule_build: git_submodule_build_opus git_submodule_build_rtaudiowrapper

git_submodule_build_opus:
	cd internal/opus && go run build.go
		
git_submodule_build_rtaudiowrapper:
	cd internal/rtaudiowrapper && \
		go generate . && \
		go build -o ../../bin/rtaudiowrapper .


# --------------------------------------------------------------------------------
# Build 
# For building the client
# 
# Example building can be found in the respective example directory

build:
	go build .

# TODO: Tags? rtaudio include/exclude tag?

clean:
	rm bin/*
