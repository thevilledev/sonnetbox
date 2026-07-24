function(environment)
  local production = environment == "production";
  {
    environment: environment,
    service: {
      name: "catalog",
      replicas: if production then 3 else 1,
      logLevel: if production then "info" else "debug",
    },
  }
