#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# Generate configs for the plugin
#
# Note: For Two Six Direct Links, we call the generate_configs.py script. This
# file is a wrapped to have a standardized bash interface
#
# Example Call:
#    bash generate_configs.sh {arguments}
# -----------------------------------------------------------------------------


set -e


###
# Arguments
###


# Get Path
BASE_DIR=$(cd $(dirname ${BASH_SOURCE[0]}) >/dev/null 2>&1 && pwd)


###
# Main Execution
###


python3 ${BASE_DIR}/generate_configs.py "$@"
