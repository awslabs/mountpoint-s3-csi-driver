RENDERED_HELM=$(helm template charts/aws-mountpoint-s3-csi-driver $HELM_ARGS)
IMAGES=$(echo "$RENDERED_HELM" | yq -o json -r 'select(.kind == "DaemonSet" and .metadata.name == "s3-csi-node") | .spec.template.spec.containers[] | select(.name != "s3-plugin") | .image')

for IMAGE in $IMAGES;
do
  if docker manifest inspect -v "$IMAGE" > /dev/null; then
    echo "Found image: $IMAGE"
  else
    echo "Did not find image: $IMAGE"
    exit 1
  fi
done
