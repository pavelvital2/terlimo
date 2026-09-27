-- Reverting 0033 removes only the devices-list revision metadata; no product data is affected.
DROP TABLE device_list_revisions;
