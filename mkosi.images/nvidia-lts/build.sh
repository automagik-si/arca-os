#!/bin/sh -eux

export NVIDIA_BRANCH_DIR=/work/src/mkosi.images/nvidia-lts
exec /work/src/mkosi.images/nvidia/build.sh
