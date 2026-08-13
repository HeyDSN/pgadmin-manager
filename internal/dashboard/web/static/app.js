const confirmDialog = document.querySelector("#confirm-dialog");
const confirmTitle = confirmDialog?.querySelector("#confirm-title");
const confirmMessage = confirmDialog?.querySelector("#confirm-message");
const confirmSubmit = confirmDialog?.querySelector("[data-confirm-submit]");
const confirmCancel = confirmDialog?.querySelector("[data-confirm-cancel]");
let pendingForm = null;

document.addEventListener("submit", function (event) {
  const form = event.target.closest("form.js-confirm");
  if (!form || form.dataset.confirmed === "true") return;

  event.preventDefault();
  pendingForm = form;
  confirmTitle.textContent = form.dataset.confirmTitle || "Confirm action?";
  confirmMessage.textContent = form.dataset.confirm || "This action cannot be undone.";
  confirmDialog.showModal();
  confirmCancel.focus();
});

confirmCancel?.addEventListener("click", function () {
  pendingForm = null;
  confirmDialog.close();
});

confirmSubmit?.addEventListener("click", function () {
  if (!pendingForm) return;
  const form = pendingForm;
  pendingForm = null;
  form.dataset.confirmed = "true";
  const confirmation = form.querySelector('input[name="confirmed"]');
  if (confirmation) confirmation.value = "yes";
  confirmDialog.close();
  form.requestSubmit();
});

confirmDialog?.addEventListener("cancel", function () {
  pendingForm = null;
});

document.querySelectorAll("dialog[data-auto-open]").forEach(function (dialog) {
  dialog.showModal();
});

document.querySelectorAll("[data-table-search]").forEach(function (input) {
  const table = document.getElementById(input.dataset.tableSearch);
  const body = table?.tBodies[0];
  if (!body) return;

  const rows = Array.from(body.rows).filter(function (row) {
    return !row.querySelector(".empty");
  });
  const noResults = document.createElement("tr");
  noResults.className = "search-empty";
  noResults.hidden = true;
  noResults.innerHTML = `<td colspan="${table.tHead.rows[0].cells.length}" class="empty">No matching results</td>`;
  body.appendChild(noResults);

  input.addEventListener("input", function () {
    const query = input.value.trim().toLocaleLowerCase();
    let visible = 0;
    rows.forEach(function (row) {
      const matches = !query || row.textContent.toLocaleLowerCase().includes(query);
      row.hidden = !matches;
      if (matches) visible++;
    });
    noResults.hidden = visible !== 0;
  });
});
