"use strict";
const $ = (s) => document.querySelector(s);
const S = {
  user: null,
  csrf: "",
  view: "messages",
  generation: 0,
  cursors: [],
  after: "",
  page: null,
};
const titles = {
  messages: "邮件记录",
  providers: "发送通道",
  suppressions: "地址抑制",
  access: "访问与说明",
};
const roles = { admin: "管理员", operator: "操作员", viewer: "只读观察员" };
const states = {
  IN_PROGRESS: ["正在投递", "accepted"],
  QUEUED: ["等待投递", ""],
  SENDING: ["正在投递", "accepted"],
  SMTP_ACCEPTED: ["SMTP 已接受", "accepted"],
  PARTIAL_ACCEPTED: ["部分已接受", "warn"],
  TEMP_FAILED: ["暂时失败", "warn"],
  PERM_FAILED: ["永久失败", "bad"],
  DELIVERY_UNKNOWN: ["结果待确认", "warn"],
  DELIVERED: ["已确认收到", "good"],
  BOUNCED: ["已退信", "bad"],
  SUPPRESSED: ["已抑制", ""],
  CANCELLED: ["已取消", ""],
  RECEIVED: ["已接收", ""],
  ARCHIVED: ["已存档", ""],
  HEALTHY: ["健康", "good"],
  DEGRADED: ["降级", "warn"],
  UNHEALTHY: ["不可用", "bad"],
  DISABLED: ["已停用", ""],
  CLOSED: ["熔断关闭", ""],
  OPEN: ["熔断中", "warn"],
  HALF_OPEN: ["半开探测", "warn"],
};
const eventNames = {
  QUOTA_RESERVED: "预留投递额度",
  DNS_RESOLVED: "地址解析完成",
  SMTP_GREETING: "收到 SMTP 问候",
  EHLO_COMPLETED: "确认服务能力",
  AUTH_STARTED: "开始身份认证",
  QUIT_SENT: "结束 SMTP 会话",
  RCPT_REJECTED: "收件人被拒绝",
  DELIVERY_DECIDED: "记录收件人投递决策",
  DELIVERY_UNKNOWN: "投递结果待人工确认",

  MESSAGE_RECEIVED: "网关已接收",
  MESSAGE_ARCHIVED: "原文已存档",
  MESSAGE_QUEUED: "进入投递队列",
  ATTEMPT_STARTED: "开始投递尝试",
  DNS_STARTED: "开始解析地址",
  DNS_COMPLETED: "地址解析完成",
  TCP_CONNECTED: "建立连接",
  TLS_ESTABLISHED: "TLS 已建立",
  AUTH_SUCCEEDED: "认证通过",
  MAIL_FROM_ACCEPTED: "发件人已接受",
  RCPT_ACCEPTED: "收件人已接受",
  DATA_ARMED: "已授权传输正文",
  DATA_STARTED: "开始传输正文",
  DATA_COMPLETED: "正文传输完成",
  SMTP_ACCEPTED: "SMTP 已接受",
  ATTEMPT_FAILED: "投递尝试失败",
  UNKNOWN_RESOLVED: "人工处置已记录",
  RECIPIENT_SUPPRESSED: "收件地址已抑制",
  RETRY_QUEUED: "重试已入队",
};
function el(tag, props = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v == null) continue;
    if (k.startsWith("on")) n.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === "class") n.className = v;
    else if (k === "text") n.textContent = v;
    else if (k === "value") n.value = v;
    else if (k === "checked") n.checked = v;
    else n.setAttribute(k, v);
  }
  for (const c of children.flat(Infinity)) {
    if (c != null)
      n.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return n;
}
const button = (text, fn, cls = "") =>
  el("button", { type: "button", class: cls, onClick: fn }, text);
const badge = (status) => {
  const v = states[status] || [status || "尚无观测", ""];
  return el("span", { class: "badge " + v[1] }, v[0]);
};
const date = (s) =>
  s ? new Date(s).toLocaleString("zh-CN", { hour12: false }) : "—";
const canWrite = () => S.user?.role !== "viewer";
const canAdmin = () => S.user?.role === "admin";
function problem(message) {
  $("#notice").hidden = !message;
  $("#notice").className = "notice";
  $("#notice").textContent = message || "";
}
function errorText(e) {
  return (
    {
      401: "会话已失效，请重新登录。",
      403: "没有此操作的权限，或请求来源验证未通过。",
      409: "对象已变化或不满足处理条件。请刷新后重新核对，不要重复提交。",
      400: "请检查输入内容和必填字段。",
      415: "请求格式不受支持。",
      429: "登录尝试过于频繁，请稍后再试。",
      503: "操作未能确认完成。请刷新核对对象状态，再决定是否继续。",
    }[e.status] || "连接中断，未能确认结果。请刷新核对状态后再继续。"
  );
}
async function request(path, options = {}) {
  const method = options.method || "GET";
  const headers = {};
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") headers["X-CSRF-Token"] = S.csrf;
  let r;
  try {
    r = await fetch("/console/" + path, {
      method,
      headers,
      credentials: "same-origin",
      cache: "no-store",
      body:
        options.body === undefined ? undefined : JSON.stringify(options.body),
    });
  } catch {
    throw new Error("connection");
  }
  if (!r.ok) {
    const e = new Error("request failed");
    e.status = r.status;
    if (r.status === 401 && path !== "login" && path !== "session") {
      signedOut();
    }
    throw e;
  }
  const payload = await r.json();
  if (S.user) armSession();
  return payload;
}
const api = (path, body, method) =>
  request("api/" + path, {
    body,
    method: method || (body === undefined ? "GET" : "POST"),
  });
function signedOut() {
  clearTimeout(S.timer);
  S.generation++;
  S.user = null;
  S.csrf = "";
  S.page = null;
  S.cursors = [];
  S.after = "";
  $("#view").replaceChildren();
  $("#notice").textContent = "";
  $("#modal").close();
  $("#modal").replaceChildren();
  $("#shell").hidden = true;
  $("#login").hidden = false;
  $("#boot").hidden = true;
  $("#login-form").reset();
}
function armSession() {
  clearTimeout(S.timer);
  const remaining = Math.min(
    15 * 60 * 1000,
    new Date(S.user.expires_at).getTime() - Date.now(),
  );
  S.timer = setTimeout(
    () => {
      signedOut();
      $("#login-error").textContent = "会话已到期，请重新登录。";
    },
    Math.max(0, remaining),
  );
}
function signedIn(user) {
  S.user = user;
  armSession();
  S.csrf = user.csrf;
  $("#identity").textContent = user.id;
  $("#avatar").textContent = user.id[0].toUpperCase();
  $("#role").textContent = roles[user.role];
  $("#login").hidden = true;
  $("#shell").hidden = false;
  $("#boot").hidden = true;
  route();
}
$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.currentTarget;
  const b = form.querySelector("button");
  b.disabled = true;
  $("#login-error").textContent = "";
  const data = Object.fromEntries(new FormData(form));
  try {
    const user = await request("login", { method: "POST", body: data });
    form.reset();
    signedIn(user);
  } catch (err) {
    form.elements.password.value = "";
    $("#login-error").textContent =
      err.status === 401 ? "账户或密码不正确。" : errorText(err);
  } finally {
    data.password = "";
    b.disabled = false;
  }
});
async function logout() {
  try {
    await request("logout", { method: "POST", body: {} });
    signedOut();
  } catch (e) {
    problem(errorText(e));
  }
}
$("#logout").addEventListener("click", logout);
function heading(title, description, ...actions) {
  return el(
    "div",
    { class: "page-heading" },
    el("div", {}, el("h1", {}, title), el("p", {}, description)),
    el("div", { class: "actions" }, actions),
  );
}
function empty(title, description) {
  return el(
    "div",
    { class: "empty" },
    el("div", { class: "empty-symbol", "aria-hidden": "true" }, "✉"),
    el("h2", {}, title),
    el("p", {}, description),
  );
}
function facts(items) {
  return el(
    "dl",
    { class: "facts" },
    Object.entries(items).flatMap(([k, v]) => [
      el("dt", {}, k),
      el("dd", {}, v ?? "—"),
    ]),
  );
}
function pager(data, reload) {
  return el(
    "div",
    { class: "pager" },
    el(
      "p",
      {},
      `当前页 ${data.items.length} 条${data.has_more ? "，还有更多记录" : ""}`,
    ),
    el(
      "div",
      { class: "actions" },
      S.cursors.length
        ? button("上一页", () => {
            S.after = S.cursors.pop();
            reload();
          })
        : null,
      data.has_more
        ? button("下一页", () => {
            S.cursors.push(S.after);
            S.after = data.next_after;
            reload();
          })
        : null,
    ),
  );
}
function table(headers, rows) {
  for (const row of rows)
    Array.from(row.children).forEach((td, i) =>
      td.setAttribute("data-label", headers[i]),
    );
  return el(
    "div",
    { class: "table-wrap" },
    el(
      "table",
      {},
      el(
        "thead",
        {},
        el(
          "tr",
          {},
          headers.map((x) => el("th", { scope: "col" }, x)),
        ),
      ),
      el("tbody", {}, rows),
    ),
  );
}
function dialog(title, content, label, save, description = "") {
  const d = $("#modal");
  d.replaceChildren();
  let error = el("p", { class: "form-error", role: "alert" });
  const submit = el("button", { class: "primary", type: "submit" }, label);
  const cancel = button("取消", () => d.close());
  const form = el(
    "form",
    {
      onSubmit: async (e) => {
        e.preventDefault();
        if (!form.reportValidity()) return;
        submit.disabled = true;
        cancel.disabled = true;
        error.textContent = "";
        try {
          await save(new FormData(form));
          d.close();
          d.replaceChildren();
          problem("");
          route();
        } catch (err) {
          error.textContent = errorText(err);
        } finally {
          submit.disabled = false;
          cancel.disabled = false;
        }
      },
    },
    el("h2", { id: "dialog-title" }, title),
    description ? el("p", {}, description) : null,
    content,
    error,
    el("div", { class: "dialog-actions" }, cancel, submit),
  );
  d.append(form);
  d.showModal();
}
function field(label, name, value = "", type = "text", extra = {}) {
  return el(
    "label",
    { class: extra.wide ? "wide" : null },
    label,
    el(type === "textarea" ? "textarea" : "input", {
      name,
      value,
      type: type === "textarea" ? null : type,
      required: extra.optional ? null : "",
      ...extra,
    }),
  );
}
function reason() {
  return field("处理理由", "reason", "", "textarea", {
    maxlength: 2048,
    wide: true,
  });
}
function summary(name, id) {
  return el("div", { class: "object-summary" }, name, el("code", {}, id));
}
function providerForm(p = null) {
  const s = p || {
    name: "",
    host: "",
    port: 587,
    security: "starttls",
    username: "",
    priority: 10,
    max_connections: 1,
    timeout_seconds: 30,
    hourly_limit: 0,
    daily_limit: 0,
    from_domains: [],
    enabled: false,
  };
  const fields = el(
    "div",
    { class: "form-grid" },
    field("通道名称", "name", s.name, "text", { maxlength: 128 }),
    field("SMTP 主机", "host", s.host, "text", { maxlength: 253 }),
    field("端口", "port", s.port, "number", { min: 1, max: 65535 }),
    el(
      "label",
      {},
      "加密方式",
      el(
        "select",
        { name: "security" },
        ["starttls", "implicit_tls"].map((v) =>
          el(
            "option",
            { value: v, selected: v === s.security ? "" : null },
            v === "starttls" ? "STARTTLS" : "隐式 TLS",
          ),
        ),
      ),
    ),
    field("SMTP 用户名", "username", s.username, "text", { maxlength: 320 }),
    !p
      ? field("SMTP 密码", "password", "", "password", {
          autocomplete: "new-password",
          maxlength: 4096,
        })
      : null,
    field(
      "允许发件域（逗号分隔）",
      "from_domains",
      s.from_domains.join(", "),
      "text",
      { wide: true },
    ),
    field("优先级（越小越优先）", "priority", s.priority, "number", {
      min: -2147483648,
      max: 2147483647,
    }),
    field("并发连接", "max_connections", s.max_connections, "number", {
      min: 1,
      max: 32,
    }),
    field("超时秒数", "timeout_seconds", s.timeout_seconds, "number", {
      min: 1,
      max: 300,
    }),
    field("每小时额度（0 不限）", "hourly_limit", s.hourly_limit, "number", {
      min: 0,
      max: 2147483647,
    }),
    field("每日额度（0 不限）", "daily_limit", s.daily_limit, "number", {
      min: 0,
      max: 2147483647,
    }),
  );
  const content = el(
    "div",
    {},
    p ? summary(p.name, `配置版本 ${p.revision} / ${p.id}`) : null,
    fields,
    el(
      "label",
      { class: "check-label" },
      el("input", { type: "checkbox", name: "enabled", checked: s.enabled }),
      "启用通道，允许后续领取使用此配置",
    ),
    reason(),
  );
  const id = p?.id || crypto.randomUUID();
  dialog(
    p ? "编辑发送通道" : "添加发送通道",
    content,
    "保存配置",
    async (data) => {
      const settings = {};
      for (const k of ["name", "host", "security", "username"])
        settings[k] = data.get(k);
      for (const k of [
        "port",
        "priority",
        "max_connections",
        "timeout_seconds",
        "hourly_limit",
        "daily_limit",
      ])
        settings[k] = Number(data.get(k));
      settings.from_domains = data
        .get("from_domains")
        .split(",")
        .map((x) => x.trim());
      settings.enabled = data.has("enabled");
      const payload = { settings, reason: data.get("reason") };
      if (p) {
        payload.expected_revision = p.revision;
        await api("providers/" + id, payload, "PUT");
      } else {
        payload.id = id;
        payload.password = data.get("password");
        await api("providers", payload);
      }
    },
    "只影响后续领取。在途尝试不会被撤销；配置冲突时请刷新核对。",
  );
}
function rotate(p) {
  dialog(
    "轮换 SMTP 密码",
    el(
      "div",
      {},
      summary(p.name, `配置版本 ${p.revision} / ${p.id}`),
      field("新密码", "password", "", "password", {
        autocomplete: "new-password",
        maxlength: 4096,
      }),
      reason(),
    ),
    "确认轮换",
    (data) =>
      api("providers/" + p.id + "/credentials", {
        expected_revision: p.revision,
        password: data.get("password"),
        reason: data.get("reason"),
      }),
    "新密码不会回显；在途尝试仍使用原凭证快照。",
  );
}
function addSuppression() {
  dialog(
    "添加地址抑制",
    el(
      "div",
      { class: "form-grid" },
      field("邮箱地址", "email", "", "email", { maxlength: 320, wide: true }),
      el(
        "label",
        {},
        "分类",
        el(
          "select",
          { name: "category" },
          [
            ["manual", "人工抑制"],
            ["hard_bounce", "可信硬退信"],
            ["complaint", "投诉"],
            ["invalid", "无效地址"],
            ["unsubscribe", "退订"],
          ].map(([v, t]) => el("option", { value: v }, t)),
        ),
      ),
      field("到期时间（可留空）", "expires_at", "", "datetime-local", {
        optional: true,
      }),
      reason(),
    ),
    "确认添加",
    (data) =>
      api("suppressions", {
        email: data.get("email"),
        category: data.get("category"),
        expires_at: data.get("expires_at")
          ? new Date(data.get("expires_at")).toISOString()
          : null,
        reason: data.get("reason"),
      }),
    "将阻止该地址的新投递与待发送操作。已经获得正文传输许可的尝试无法撤回。",
  );
}
function release(s) {
  dialog(
    "解除地址抑制",
    el("div", {}, summary(s.email, s.id), reason()),
    "确认解除",
    (data) =>
      api("suppressions/" + s.id + "/release", { reason: data.get("reason") }),
    "解除后允许未来投递，不会自动重发历史邮件。",
  );
}
function resolveRecipient(r) {
  const select = el(
    "select",
    { name: "action" },
    el("option", { value: "mark-failed" }, "记录为永久失败"),
    el("option", { value: "mark-delivered" }, "记录为已确认收到"),
    el("option", { value: "retry" }, "重新排队投递"),
  );
  const acknowledge = el("input", { name: "acknowledge", type: "checkbox" });
  const risk = el(
    "label",
    { class: "check-label" },
    acknowledge,
    "我已核对证据，接受重新投递可能导致重复收信的风险。",
  );
  risk.hidden = true;
  select.addEventListener("change", () => {
    risk.hidden = select.value !== "retry";
    acknowledge.required = select.value === "retry";
    acknowledge.checked = false;
  });
  dialog(
    "处置待确认的投递",
    el(
      "div",
      {},
      summary(r.address, `尝试 ${r.latest_attempt_id}`),
      el(
        "p",
        { class: "callout warn" },
        "远端可能已接收这封邮件。此操作记录人工判断，不会改写原 SMTP 尝试。",
      ),
      el("label", {}, "处理方式", select),
      risk,
      reason(),
    ),
    "确认并记录",
    (data) =>
      api("recipients/" + r.id + "/resolve-unknown", {
        expected_attempt: r.latest_attempt_id,
        action: data.get("action"),
        reason: data.get("reason"),
        acknowledge_duplicate_risk: data.has("acknowledge"),
      }),
  );
}
async function listPage(view, generation) {
  const path =
    view === "messages"
      ? "messages"
      : view === "providers"
        ? "providers"
        : "suppressions";
  const data = await api(
    path +
      "?limit=50" +
      (S.after ? "&after=" + encodeURIComponent(S.after) : ""),
  );
  if (S.generation !== generation) return;
  S.page = data;
  const host = $("#view");
  host.replaceChildren();
  const refresh = () => {
    S.after = "";
    S.cursors = [];
    route(false);
  };
  host.append(
    heading(
      titles[view],
      view === "messages"
        ? "从接收到确认，查看每封邮件的投递事实。"
        : view === "providers"
          ? "管理投递通道，查看健康观测与预留用量。"
          : "保护收件人信誉，保留每一次添加和解除的记录。",
      button("刷新", refresh),
      view === "providers" && canAdmin()
        ? button("添加通道", () => providerForm(), "primary")
        : view === "suppressions" && canWrite()
          ? button("添加抑制", addSuppression, "primary")
          : null,
    ),
  );
  if (view === "providers") {
    const grid = el("div", { class: "provider-grid" });
    for (const p of data.items) {
      grid.append(
        el(
          "article",
          { class: "panel provider-card" },
          el("h2", {}, p.name),
          el("p", { class: "host" }, `${p.host}:${p.port}`),
          el(
            "div",
            { class: "badges" },
            badge(p.enabled ? p.health_status || "尚无观测" : "DISABLED"),
            p.circuit_state ? badge(p.circuit_state) : null,
          ),
          el(
            "div",
            { class: "usage" },
            el(
              "div",
              {},
              el("strong", {}, p.hourly_reserved),
              el("small", {}, `小时预留 / ${p.hourly_limit || "不限"}`),
            ),
            el(
              "div",
              {},
              el("strong", {}, p.daily_reserved),
              el("small", {}, `日预留 / ${p.daily_limit || "不限"}`),
            ),
          ),
          el("p", { class: "host" }, "最近成功：" + date(p.last_success_at)),
          el("p", { class: "host" }, "熔断恢复：" + date(p.open_until)),
          el(
            "div",
            { class: "actions" },
            button("查看配置", () => providerDetail(p)),
            canAdmin() ? button("编辑", () => providerForm(p)) : null,
            canAdmin() ? button("轮换密码", () => rotate(p)) : null,
          ),
        ),
      );
    }
    host.append(
      grid.childNodes.length
        ? grid
        : el(
            "div",
            { class: "panel" },
            empty(
              "尚未配置发送通道",
              "添加 SMTP Provider 后，符合路由资格的邮件才能通过该通道发送。",
            ),
          ),
      pager(data, () => route(false)),
    );
    return;
  }
  const panel = el("div", { class: "panel" });
  const input = el("input", {
    type: "search",
    placeholder:
      view === "messages" ? "筛选当前页的主题或发件人" : "筛选当前页的地址",
    "aria-label": "筛选当前页",
  });
  const select = el(
    "select",
    { "aria-label": "筛选状态" },
    el("option", { value: "" }, "所有状态"),
    ...(view === "messages"
      ? Object.keys(states)
          .filter((k) =>
            [
              "QUEUED",
              "SENDING",
              "SMTP_ACCEPTED",
              "PARTIAL_ACCEPTED",
              "TEMP_FAILED",
              "PERM_FAILED",
              "DELIVERY_UNKNOWN",
              "DELIVERED",
              "BOUNCED",
              "SUPPRESSED",
              "CANCELLED",
            ].includes(k),
          )
          .map((k) => el("option", { value: k }, states[k][0]))
      : [
          el("option", { value: "active" }, "生效中"),
          el("option", { value: "inactive" }, "历史记录"),
        ]),
  );
  const contents = el("div");
  function renderRows() {
    const q = input.value.toLowerCase();
    const items = data.items.filter(
      (x) =>
        JSON.stringify(
          view === "messages" ? [x.subject, x.envelope_from, x.id] : [x.email],
        )
          .toLowerCase()
          .includes(q) &&
        (!select.value ||
          (view === "messages"
            ? x.status === select.value
            : (select.value === "active") === x.active)),
    );
    const rows = items.map((x) =>
      view === "messages"
        ? el(
            "tr",
            {},
            el("td", {}, badge(x.status)),
            el(
              "td",
              {},
              button(
                x.subject || "（无主题）",
                () => {
                  location.hash = "messages/" + x.id;
                },
                "subject",
              ),
              el("small", {}, x.envelope_from),
            ),
            el("td", {}, date(x.created_at)),
            el("td", {}, x.attempt_count),
          )
        : el(
            "tr",
            {},
            el("td", {}, el("strong", {}, x.email), el("small", {}, x.note)),
            el("td", {}, badge(x.active ? "生效中" : "历史记录")),
            el("td", {}, x.category, el("small", {}, x.actor)),
            el("td", {}, date(x.expires_at)),
            el(
              "td",
              {},
              canWrite() && x.active ? button("解除", () => release(x)) : null,
            ),
          ),
    );
    contents.replaceChildren(
      rows.length
        ? table(
            view === "messages"
              ? ["投递状态", "邮件 / 发件人", "接收时间", "尝试"]
              : [
                  "地址 / 处理理由",
                  "状态",
                  "分类 / 操作者",
                  "到期时间",
                  "操作",
                ],
            rows,
          )
        : empty(
            data.items.length
              ? "没有匹配的记录"
              : view === "messages"
                ? "还没有邮件记录"
                : "尚无抑制记录",
            data.items.length
              ? "调整当前页筛选，或翻页查看更多记录。"
              : view === "messages"
                ? "通过已配置的 SMTP 账户提交邮件后，记录会显示在这里。"
                : "需要暂停向某个地址投递时，添加抑制并留下处理理由。",
          ),
    );
  }
  input.addEventListener("input", renderRows);
  select.addEventListener("change", renderRows);
  panel.append(
    el(
      "div",
      { class: "toolbar" },
      input,
      select,
      el("small", {}, "筛选仅作用于当前页"),
    ),
    contents,
    pager(data, () => route(false)),
  );
  host.append(panel);
  renderRows();
}
function providerDetail(p) {
  const content = el(
    "div",
    {},
    facts({
      通道: p.name,
      配置版本: p.revision,
      主机: p.host,
      "端口 / 加密": `${p.port} / ${p.security}`,
      用户名: p.username,
      允许发件域: p.from_domains.join(", "),
      优先级: p.priority,
      最大连接: p.max_connections,
      超时: p.timeout_seconds + " 秒",
      最近失败: date(p.last_failure_at),
    }),
  );
  const d = $("#modal");
  d.replaceChildren(
    el(
      "form",
      { method: "dialog" },
      el("h2", { id: "dialog-title" }, "通道配置"),
      content,
      el(
        "div",
        { class: "dialog-actions" },
        button("关闭", () => d.close()),
      ),
    ),
  );
  d.showModal();
}
async function messageDetail(id, generation) {
  const [m, rs, ats, evs] = await Promise.all([
    api("messages/" + id),
    api("messages/" + id + "/recipients?limit=100"),
    api("messages/" + id + "/attempts?limit=100"),
    api("messages/" + id + "/events?limit=100"),
  ]);
  if (S.generation !== generation) return;
  const host = $("#view");
  host.replaceChildren(
    heading(
      "投递详情",
      "从持久化记录还原过程，不推断收件结果。",
      button("返回邮件", () => {
        location.hash = "messages";
      }),
      button("刷新", () => route(false)),
    ),
  );
  const recipients = el(
    "section",
    { class: "detail-section" },
    el("h2", {}, "收件人"),
  );
  const timeline = el("ol", {
    class: "timeline",
    tabindex: "0",
    "aria-label": "投递事件记录",
  });
  const attempts = el(
    "section",
    { class: "detail-section" },
    el("h2", {}, "尝试记录"),
  );
  const addRecipients = (items) => {
    for (const r of items)
      recipients.append(
        el(
          "article",
          { class: "recipient" },
          el(
            "div",
            { class: "recipient-title" },
            el("strong", {}, r.address),
            badge(r.status),
          ),
          el(
            "small",
            { class: "muted" },
            r.smtp_code ? "SMTP " + r.smtp_code : "尚无 SMTP 返回码",
          ),
          r.status === "DELIVERY_UNKNOWN" && r.latest_attempt_id && canWrite()
            ? el(
                "div",
                { class: "actions" },
                button("人工处置", () => resolveRecipient(r)),
              )
            : null,
        ),
      );
  };
  const addEvents = (items) => {
    for (const e of items)
      timeline.append(
        el(
          "li",
          {},
          el("strong", {}, eventNames[e.event_type] || e.event_type),
          el("time", { datetime: e.event_time }, date(e.event_time)),
          el(
            "small",
            {},
            e.source +
              (e.attempt_sequence ? " / 序号 " + e.attempt_sequence : ""),
          ),
        ),
      );
  };
  const addAttempts = (items) => {
    for (const a of items)
      attempts.append(
        el(
          "article",
          { class: "recipient" },
          el(
            "div",
            { class: "recipient-title" },
            el("strong", {}, "尝试 " + a.attempt_number),
            badge(a.result),
          ),
          el("small", { class: "muted" }, date(a.started_at)),
          el("p", { class: "mono" }, a.id),
          a.error_class ? el("small", { class: "muted" }, a.error_class) : null,
        ),
      );
  };
  async function more(kind, page, target, append) {
    if (!page.has_more) return;
    const b = button("加载更多", async () => {
      b.disabled = true;
      try {
        const next = await api(
          `messages/${id}/${kind}?limit=100&after=${encodeURIComponent(page.next_after)}`,
        );
        if (S.generation !== generation) return;
        b.remove();
        append(next.items);
        more(kind, next, target, append);
      } catch (e) {
        b.disabled = false;
        problem(errorText(e));
      }
    });
    target.append(b);
  }
  addRecipients(rs.items);
  addEvents(evs.items);
  addAttempts(ats.items);
  more("recipients", rs, recipients, addRecipients);
  more("attempts", ats, attempts, addAttempts);
  if (!rs.items.length)
    recipients.append(el("p", { class: "muted" }, "暂无收件人记录"));
  if (!ats.items.length)
    attempts.append(el("p", { class: "muted" }, "尚未开始投递尝试"));
  const timeSection = el(
    "section",
    { class: "detail-section" },
    el("h2", {}, "事件时间线"),
    timeline,
  );
  if (!evs.items.length)
    timeSection.append(el("p", { class: "muted" }, "暂无事件"));
  more("events", evs, timeSection, addEvents);
  host.append(
    el(
      "div",
      { class: "detail-layout" },
      el(
        "div",
        { class: "panel" },
        el(
          "section",
          { class: "detail-top" },
          badge(m.status),
          el("h2", {}, m.subject || "（无主题）"),
          el("p", { class: "mono" }, m.id),
          el(
            "p",
            {
              class:
                "callout" + (m.status === "DELIVERY_UNKNOWN" ? " warn" : ""),
            },
            m.status === "DELIVERY_UNKNOWN"
              ? "远端可能已经接收。系统不会自动重发，请先核对收件情况。"
              : "SMTP 接受仅代表远端接受了邮件，不等于收件人已收到。",
          ),
        ),
        el(
          "section",
          { class: "detail-section" },
          facts({
            发件人: m.envelope_from,
            "原 Message-ID": m.message_id || "未提供",
            接收时间: date(m.created_at),
            大小: (m.eml_size / 1024).toFixed(1) + " KiB",
            存档:
              m.archive_state === "AVAILABLE" ? "原文可用" : m.archive_state,
          }),
        ),
        recipients,
        el(
          "section",
          { class: "detail-section" },
          el("h2", {}, "原文与附件"),
          el(
            "p",
            { class: "muted" },
            "此工作台仅显示邮件元数据。正文、附件和外部图片不会在浏览器中加载。",
          ),
        ),
      ),
      el("div", { class: "panel" }, timeSection, attempts),
    ),
  );
}
function access() {
  const host = $("#view");
  host.replaceChildren(
    heading(
      "访问与说明",
      "查看当前身份与操作边界。",
      button("退出登录", logout),
    ),
    el(
      "div",
      { class: "panel" },
      el(
        "section",
        { class: "section-copy" },
        el("h2", {}, "当前会话"),
        facts({
          账户: S.user.id,
          角色: roles[S.user.role],
          会话有效期: "最长 30 分钟；闲置 15 分钟后失效",
          操作审计: "操作者由登录身份确定",
        }),
      ),
      el(
        "section",
        { class: "section-copy" },
        el("h2", {}, "投递状态的含义"),
        el(
          "p",
          {},
          "SMTP 已接受：远端 SMTP 返回接受结果，尚不能据此确认入箱。",
        ),
        el("p", {}, "结果待确认：远端可能已接收，系统不会自动重发。"),
        el(
          "p",
          {},
          "已确认收到：人工根据外部证据作出的记录，不会覆盖原始 SMTP 尝试。",
        ),
      ),
      el(
        "section",
        { class: "section-copy" },
        el("h2", {}, "账户与安全"),
        el(
          "p",
          {},
          "账户由部署管理员配置，退出或服务重启会使当前会话失效。工作台不保存长期管理 API 令牌。",
        ),
        el(
          "p",
          {},
          "SMTP 账户及系统参数通过部署配置和本地管理工具维护。此页面不会伪装成尚未实现的系统设置。",
        ),
      ),
    ),
  );
}
function route(reset = true) {
  if (!S.user) return;
  const parts = location.hash.slice(1).split("/");
  const view = titles[parts[0]] ? parts[0] : "messages";
  if (reset || S.view !== view) {
    S.after = "";
    S.cursors = [];
  }
  S.view = view;
  const generation = ++S.generation;
  problem("");
  document.querySelectorAll("nav a").forEach((a) => {
    a.classList.toggle("active", a.dataset.view === view);
    if (a.dataset.view === view) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  });
  $("#breadcrumb").textContent = titles[view];
  $("#view").replaceChildren(empty("正在读取记录", "请稍候…"));
  let work;
  if (view === "access") {
    access();
    return;
  }
  if (view === "messages" && parts[1])
    work = messageDetail(parts[1], generation);
  else work = listPage(view, generation);
  work.catch((e) => {
    if (S.generation !== generation) return;
    problem(errorText(e));
    $("#view").replaceChildren(
      heading(
        titles[view],
        "记录暂时无法读取。",
        button("重新加载", () => route(false)),
      ),
      el(
        "div",
        { class: "panel" },
        empty(
          "未能加载记录",
          "连接恢复后重新加载。此处不会显示未经确认的结果。",
        ),
      ),
    );
  });
}
window.addEventListener("hashchange", () => route());
request("session")
  .then(signedIn)
  .catch((e) => {
    signedOut();
    if (e.status !== 401) $("#login-error").textContent = errorText(e);
  });
$("#modal").addEventListener("close", () => {
  if (!$("#modal").open) $("#modal").replaceChildren();
});
