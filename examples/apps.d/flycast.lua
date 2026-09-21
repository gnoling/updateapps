-- flycast nightly builds live in a public S3 bucket; the builds web page is just
-- JavaScript that renders this listing. Pick the newest Linux AppImage on master.
local bucket = "https://flycast-builds.s3.fr-par.scw.cloud/"

local xml, status = http.get(bucket .. "?prefix=linux/heads/master-")
if status ~= 200 then error("bucket listing returned HTTP " .. status) end

-- Every <Contents> has one <Key> and one <LastModified>, so the lists line up.
local keys = html.select(xml, "contents > key")
local stamps = html.select(xml, "contents > lastmodified")

local newest
for i, key in ipairs(keys) do
  local wanted = key:match("^linux/heads/master%-%x+/flycast%-x86_64%.AppImage$")
  if wanted and (not newest or stamps[i] > stamps[newest]) then
    newest = i
  end
end
if not newest then return nil, "no Linux build in the master listing" end

local commit = keys[newest]:match("master%-(%x+)/")
log.info("newest master build: " .. commit .. " (" .. stamps[newest] .. ")")
return { version = commit, display = commit:sub(1, 7), url = bucket .. keys[newest] }
