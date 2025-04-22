# thanos-limits-controller
A Kubernetes based dynamic controller to manage the limits within a Thanos Router/Receiver setup

## Project Goal

Currently when running Thanos within a Router/Receiver setup, it is _only_ possible to generate a configuration of limits based upon a given number of **static** receivers. The aim of this project is to allow that limit configuration to dynamically grow/shrink depending of the vertical/horizontal scale of the receiver at any given time.:W
