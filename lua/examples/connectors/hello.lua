-- Example connector: writes a "brief" record every minute.
connector "hello" {
  every = "1m",
  run = function(ctx)
    local id = "hello-" .. ctx.today()
    ctx.write("brief", id, { text = "hello from kron at " .. ctx.now() }, { source = "kron:hello" })
  end,
}
