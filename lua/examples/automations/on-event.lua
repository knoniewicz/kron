-- Example automation: every example.* publication is recorded as a "brief" note.
automation "note-example" {
  on = "example.*",
  run = function(ctx, event)
    local id = "example-" .. ctx.sha1(event.subject .. ctx.json_encode(event.payload))
    ctx.write("brief", id, { text = event.subject .. ": " .. ctx.json_encode(event.payload) },
              { source = "kron:note-example" })
  end,
}
