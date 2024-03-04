#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# Stop internal (running in a RACE node) services running on a RACE node 
# required by channel.
#
# Note: For Two Six Direct Links, there are no internal requirements
#
# Arguments:
# -h, --help
#     Print help and exit
#
# Example Call:
#    bash stop_internal_services.sh \
#        {--help}
# -----------------------------------------------------------------------------


###
# Helper functions
###


# Load Helper Functions
BASE_DIR=$(cd $(dirname ${BASH_SOURCE[0]}) >/dev/null 2>&1 && pwd)
. ${BASE_DIR}/helper_functions.sh


###
# Arguments
###


# Parse CLI Arguments
while [ $# -gt 0 ]
do
    key="$1"

    case $key in
        -h|--help)
        echo "Example Call: bash stop_internal_services.sh"
        exit 1;
        ;;
        *)
        formatlog "ERROR" "unknown argument \"$1\""
        exit 1
        ;;
    esac
done


###
# Main Execution
###


# N/A
