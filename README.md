aws-config-elbv2
================

Build
-----

    make build

Description
-----------

This service is going to watch etcd keys and configure ALBv2 target groups
according to the configuration in etcd. etcd keys are:

- `/aws-config-elbv2/target-group/*/`: contains the configuration for a target
  group. The last path component is a opaque unique id for the target group.

- `/aws-config-elbv2/target-group/*/arn`: contains the itarget group ARN

- `/aws-config-elbv2/target-group/*/region`: contains the itarget group region.
  Will use the default region if not specified

- `/aws-config-elbv2/target-group/*/attachments/*`: a JSON describing an
  attachment containing `id`: the ECD instance identifier and `port`: the port
  on which to connect.

Changes are detected live and are applied immediatly.
