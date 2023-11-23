#!/bin/bash

set -euox pipefail

N=2
BASE_DIR=$(dirname "$(realpath "${BASH_SOURCE[0]}")")
INSTALL=${INSTALL:-0}
CFG=${CFG:-seq_read}

if [[ "${INSTALL}" == "1" ]]; then
    for i in $(seq $N)
    do
        CUSTOMER_POD_NAME=s3-app-$i
        kubectl exec ${CUSTOMER_POD_NAME} -- bash -c "apt-get update && apt-get install fio -y"
    done
fi

for i in $(seq $N)
do
    CUSTOMER_POD_NAME=s3-app-$i
    kubectl cp ${BASE_DIR}/${CFG}.fio default/${CUSTOMER_POD_NAME}:/c.fio
done

for i in $(seq $N)
do
    CUSTOMER_POD_NAME=s3-app-$i
    kubectl exec ${CUSTOMER_POD_NAME} -- bash -c "JOB_ID=${i} fio c.fio > fio.log 2>&1" &
done

for job in `jobs -p`
do
    wait $job || let "FAIL+=1"
done

for i in $(seq $N)
do
    CUSTOMER_POD_NAME=s3-app-$i
    kubectl exec ${CUSTOMER_POD_NAME} -- cat fio.log > ${CFG}_${i}.log
done

for i in $(seq $N)
do
    aws s3 rm s3://s3-csi-perf-test-vlaad/file${i}
done
