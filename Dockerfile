FROM scratch
LABEL name="aws-config-elbv2"
LABEL version=1.0
MAINTAINER SquareScale Engineering <engineering@squarescale>
COPY aws-config-elbv2-linux-static /aws-config-elbv2
EXPOSE 80
CMD ["/aws-config-elbv2", "-listen", ":80"]
